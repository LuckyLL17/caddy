// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyhttp

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
)

// fallbackResponseWriter buffers the complete response of one branch
// attempt so that it can be discarded without the client seeing any of
// it if another branch needs to be tried. Unlike responseRecorder, it
// owns an isolated copy of the response header map: nothing written by
// a failed attempt leaks onto the underlying ResponseWriter.
//
// Some events cannot be taken back once they touch the connection. A
// 1xx response, a connection hijack, and exceeding the buffer limit all
// "commit" the attempt: buffered bytes are replayed and subsequent
// writes stream straight through, making further fallbacks impossible.
// Flush is held back while buffering and serviced after commit.
type fallbackResponseWriter struct {
	// embedded for Push and ReadFrom passthrough; Header, WriteHeader,
	// Write, ReadFrom, FlushError and Hijack are shadowed below
	*ResponseWriterWrapper

	// baseline is a copy of the underlying headers at the start of
	// the attempt; used to honor header deletions at commit time
	baseline http.Header
	header   http.Header

	statusCode  int
	wroteHeader bool

	buf     *bytes.Buffer
	maxBody int64 // 0 means the response is buffered without a limit

	spilled            bool // bytes are already streaming to the underlying writer
	committed          bool // irreversible I/O happened (spill, 1xx or hijack)
	finalHeaderPending bool // a 1xx was forwarded, the final status is still due
	spillErr           error
}

// newFallbackResponseWriter returns a response writer that isolates the
// complete response from w, buffering into buf (which must be empty).
// maxBody is the maximum number of buffered response body bytes; 0
// removes the limit.
func newFallbackResponseWriter(w http.ResponseWriter, buf *bytes.Buffer, maxBody int64) *fallbackResponseWriter {
	baseline := w.Header().Clone()
	return &fallbackResponseWriter{
		ResponseWriterWrapper: &ResponseWriterWrapper{ResponseWriter: w},
		baseline:              baseline,
		header:                baseline.Clone(),
		buf:                   buf,
		maxBody:               maxBody,
	}
}

// Header returns the isolated response headers for the attempt.
func (fw *fallbackResponseWriter) Header() http.Header {
	return fw.header
}

// WriteHeader records the status code. Informational 1xx responses
// (including 101 Switching Protocols) are already meaningful on the
// wire and cannot be buffered, so they commit the attempt immediately.
func (fw *fallbackResponseWriter) WriteHeader(statusCode int) {
	if fw.wroteHeader {
		// after forwarding a 1xx, the handler still owes the
		// final status code; pass it straight through
		if fw.finalHeaderPending && statusCode >= 200 {
			fw.finalHeaderPending = false
			fw.statusCode = statusCode
			fw.ResponseWriterWrapper.WriteHeader(statusCode)
		}
		return
	}
	fw.wroteHeader = true
	fw.statusCode = statusCode

	if statusCode >= 100 && statusCode < 200 {
		fw.spill(statusCode)
	}
}

// Write buffers the response body until the attempt is committed, or
// streams it directly after commit. If buffering would exceed maxBody,
// the attempt is committed by spilling the buffered prefix.
func (fw *fallbackResponseWriter) Write(data []byte) (int, error) {
	if !fw.wroteHeader {
		fw.WriteHeader(http.StatusOK)
	}
	if fw.spillErr != nil {
		return 0, fw.spillErr
	}
	if fw.spilled {
		return fw.ResponseWriterWrapper.Write(data)
	}
	if fw.maxBody > 0 && int64(fw.buf.Len())+int64(len(data)) > fw.maxBody {
		fw.spill(fw.statusCode)
		if fw.spillErr != nil {
			return 0, fw.spillErr
		}
		return fw.ResponseWriterWrapper.Write(data)
	}
	return fw.buf.Write(data)
}

// ReadFrom implements io.ReaderFrom, routing through Write so that the
// buffering limit and spilling behavior are preserved.
func (fw *fallbackResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !fw.wroteHeader {
		fw.WriteHeader(http.StatusOK)
	}
	if fw.spillErr != nil {
		return 0, fw.spillErr
	}
	if fw.spilled {
		return fw.ResponseWriterWrapper.ReadFrom(r)
	}
	return io.Copy(fallbackBufferedWriter{fw}, r)
}

// Status returns the recorded status code, or 0 if none was written.
func (fw *fallbackResponseWriter) Status() int {
	return fw.statusCode
}

// Wrote returns true if a final (>= 2xx) response was started.
func (fw *fallbackResponseWriter) Wrote() bool {
	return fw.wroteHeader
}

// Committed returns true if the attempt can no longer be discarded.
func (fw *fallbackResponseWriter) Committed() bool {
	return fw.committed
}

// FlushError holds flushes back while the response is buffered so that
// buffering is not defeated. After commit, flushes are passed through.
func (fw *fallbackResponseWriter) FlushError() error {
	if fw.spilled {
		//nolint:bodyclose
		return http.NewResponseController(fw.ResponseWriterWrapper).Flush()
	}
	return nil
}

// Hijack cannot be buffered: the handler takes ownership of the raw
// connection. It is passed straight through and commits the attempt.
func (fw *fallbackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	//nolint:bodyclose
	conn, brw, err := http.NewResponseController(fw.ResponseWriterWrapper).Hijack()
	if err != nil {
		return nil, nil, err
	}
	fw.spilled = true
	fw.committed = true
	fw.wroteHeader = true
	return conn, brw, nil
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (fw *fallbackResponseWriter) Unwrap() http.ResponseWriter {
	return fw.ResponseWriterWrapper
}

// commit replays the buffered status, headers, body and trailers onto
// the underlying response writer. Attempts that already spilled or were
// hijacked require nothing further.
func (fw *fallbackResponseWriter) commit() error {
	if fw.committed {
		return fw.spillErr
	}
	if !fw.wroteHeader {
		// nothing was produced; nothing to replay
		return nil
	}
	fw.committed = true

	dst := fw.ResponseWriterWrapper
	dstHeader := dst.Header()

	// honor headers the branch explicitly removed
	for k := range fw.baseline {
		if _, ok := fw.header[k]; !ok {
			delete(dstHeader, k)
		}
	}

	// actual trailers are written after the body using the trailer
	// prefix convention; the "Trailer" announcement is a regular header
	var trailerKeys []string
	for k, vv := range fw.header {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			trailerKeys = append(trailerKeys, k)
			continue
		}
		dstHeader[k] = vv
	}

	dst.WriteHeader(fw.statusCode)

	if fw.buf.Len() > 0 {
		if _, err := io.Copy(dst, fw.buf); err != nil {
			return err
		}
	}

	if len(trailerKeys) > 0 {
		// force chunked encoding so the trailers are not dropped on
		// short bodies, then surface the trailer values
		//nolint:bodyclose
		if err := http.NewResponseController(dst).Flush(); err != nil {
			return err
		}
		for _, k := range trailerKeys {
			for _, v := range fw.header[k] {
				dstHeader.Add(k, v)
			}
		}
	}

	return nil
}

// spill commits the attempt immediately: it replays the headers and any
// buffered body prefix, then flips into streaming mode for later writes.
func (fw *fallbackResponseWriter) spill(statusCode int) {
	fw.committed = true
	fw.spilled = true
	fw.finalHeaderPending = statusCode >= 100 && statusCode < 200 && statusCode != http.StatusSwitchingProtocols

	dst := fw.ResponseWriterWrapper
	dstHeader := dst.Header()

	var trailerKeys []string
	for k, vv := range fw.header {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			trailerKeys = append(trailerKeys, k)
			continue
		}
		dstHeader[k] = vv
	}

	if statusCode > 0 {
		dst.WriteHeader(statusCode)
	}

	if fw.buf.Len() > 0 {
		if _, err := io.Copy(dst, fw.buf); err != nil {
			fw.spillErr = err
		}
	}

	for _, k := range trailerKeys {
		for _, v := range fw.header[k] {
			dstHeader.Add(k, v)
		}
	}
}

// fallbackBufferedWriter is a plain io.Writer adapter so that
// ReadFrom's io.Copy does not resolve a ReadFrom method and recurse.
type fallbackBufferedWriter struct {
	fw *fallbackResponseWriter
}

func (w fallbackBufferedWriter) Write(p []byte) (int, error) {
	return w.fw.Write(p)
}

// Interface guards
var (
	_ http.ResponseWriter = (*fallbackResponseWriter)(nil)
	_ io.ReaderFrom       = (*fallbackResponseWriter)(nil)
)
