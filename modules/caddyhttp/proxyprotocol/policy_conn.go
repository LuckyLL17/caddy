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

package proxyprotocol

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	goproxy "github.com/pires/go-proxyproto"
)

// policyListener wraps a net.Listener with deterministic PROXY protocol
// handling. It never spawns goroutines or keeps connection caches: all
// per-connection state lives in the policyConn returned from Accept.
type policyListener struct {
	net.Listener

	// headerTimeout bounds header detection; it mirrors the go-proxyproto
	// listener default (0 becomes the library default, negative disables it).
	headerTimeout time.Duration
	w             *ListenerWrapper
}

// Accept mirrors goproxy.Listener.Accept's source gating, including the
// distinction between ErrInvalidUpstream (drop this connection, keep
// accepting) and other policy errors (surface them).
func (l *policyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		mode, err := l.w.policy(goproxy.ConnPolicyOptions{
			Upstream:   conn.RemoteAddr(),
			Downstream: conn.LocalAddr(),
		})
		if err != nil {
			if closeErr := conn.Close(); closeErr != nil {
				return nil, closeErr
			}
			if errors.Is(err, goproxy.ErrInvalidUpstream) {
				continue
			}
			return nil, err
		}

		// SKIP bypasses all header processing: the raw connection is served.
		if mode == goproxy.SKIP {
			return conn, nil
		}

		return newPolicyConn(conn, l.w.compiled, mode, l.headerTimeout), nil
	}
}

// policyConn handles PROXY protocol detection for exactly one connection.
// All mutable state (the once-guard, parse error, buffered reader, deadline)
// is connection-local and dies with the connection.
type policyConn struct {
	net.Conn

	cp      *compiledPolicy
	mode    goproxy.Policy
	timeout time.Duration

	br *bufio.Reader

	readDeadline atomic.Value // time.Time requested by the caller
	once         sync.Once
	readErr      error
	header       *goproxy.Header
}

func newPolicyConn(conn net.Conn, cp *compiledPolicy, mode goproxy.Policy, headerTimeout time.Duration) *policyConn {
	return &policyConn{
		Conn:    conn,
		cp:      cp,
		mode:    mode,
		timeout: headerTimeout,
		br:      bufio.NewReaderSize(conn, cp.maxHeaderSize),
	}
}

// Read processes the header on first use, drains buffered post-header bytes
// (such as a TLS ClientHello already in flight), then reads the connection
// directly.
func (c *policyConn) Read(b []byte) (int, error) {
	if err := c.ensureHeader(); err != nil {
		return 0, err
	}
	if c.br != nil {
		if c.br.Buffered() > 0 {
			n, err := c.br.Read(b)
			if c.br.Buffered() == 0 {
				c.br = nil
			}
			return n, err
		}
		c.br = nil
	}
	return c.Conn.Read(b)
}

// Write waits for header processing, mirroring go-proxyproto behavior.
func (c *policyConn) Write(b []byte) (int, error) {
	if err := c.ensureHeader(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// RemoteAddr reports the source endpoint selected by the address policy.
func (c *policyConn) RemoteAddr() net.Addr {
	_ = c.ensureHeader()
	if c.headerApplies() && c.cp.remote == endpointFromHeader && c.header.SourceAddr != nil {
		return c.header.SourceAddr
	}
	return c.Conn.RemoteAddr()
}

// LocalAddr reports the destination endpoint selected by the address policy.
func (c *policyConn) LocalAddr() net.Addr {
	_ = c.ensureHeader()
	if c.headerApplies() && c.cp.local == endpointFromHeader && c.header.DestinationAddr != nil {
		return c.header.DestinationAddr
	}
	return c.Conn.LocalAddr()
}

// SetDeadline records and applies the caller's deadline so header detection
// can restore it after temporarily clamping the read deadline.
func (c *policyConn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	return c.Conn.SetDeadline(t)
}

// SetReadDeadline records and applies the caller's read deadline.
func (c *policyConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	return c.Conn.SetReadDeadline(t)
}

// ReadFrom preserves io.ReaderFrom support (e.g. sendfile paths) once the
// header has been processed.
func (c *policyConn) ReadFrom(r io.Reader) (int64, error) {
	if err := c.ensureHeader(); err != nil {
		return 0, err
	}
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(c.Conn, r)
}

// WriteTo first delivers buffered post-header bytes, then streams the rest of
// the connection.
func (c *policyConn) WriteTo(w io.Writer) (int64, error) {
	if err := c.ensureHeader(); err != nil {
		return 0, err
	}
	if c.br == nil {
		return io.Copy(w, c.Conn)
	}
	b := make([]byte, c.br.Buffered())
	if _, err := c.br.Read(b); err != nil {
		return 0, err
	}
	var n int64
	if nn, err := w.Write(b); err != nil {
		return int64(nn), err
	} else {
		n += int64(nn)
	}
	nn, err := io.Copy(w, c.Conn)
	return n + nn, err
}

// headerApplies reports whether the parsed header endpoints may replace the
// socket endpoints. LOCAL-command headers never do, and the IGNORE gating mode
// keeps the real socket endpoints.
func (c *policyConn) headerApplies() bool {
	return c.readErr == nil &&
		c.header != nil &&
		c.header.Command == goproxy.PROXY &&
		c.mode != goproxy.IGNORE
}

func (c *policyConn) ensureHeader() error {
	c.once.Do(func() {
		c.readErr = c.readHeader()
	})
	return c.readErr
}

// readHeader peeks and validates the header exactly once, bounding the wait
// with a temporary read deadline that never extends the caller's deadline.
func (c *policyConn) readHeader() error {
	if c.timeout > 0 {
		headerDeadline := time.Now().Add(c.timeout)
		if stored, has := c.storedReadDeadline(); has && stored.Before(headerDeadline) {
			headerDeadline = stored
		}
		if err := c.Conn.SetReadDeadline(headerDeadline); err != nil {
			return err
		}
	}

	header, size, present, timedOut, err := peekHeader(c.br, c.cp)

	if c.timeout > 0 {
		stored, _ := c.storedReadDeadline()
		restoreErr := c.Conn.SetReadDeadline(stored)
		// A failure to restore the deadline only matters when the read
		// produced no result of its own (e.g. the peer closed immediately
		// after the header); a parsed header, a parse error or a timeout
		// stays authoritative.
		if err == nil && !present && !timedOut && restoreErr != nil {
			err = restoreErr
		}
	}

	if err != nil {
		// A stream error before the signature always fails the connection;
		// signature-present errors follow the malformed-header policy.
		if !present || c.cp.malformed == headerFail {
			return err
		}
		return nil
	}

	if !present {
		if c.mode == goproxy.REQUIRE || c.cp.missing == headerFail {
			return goproxy.ErrNoProxyProtocol
		}
		return nil
	}

	if c.mode == goproxy.REJECT {
		return goproxy.ErrSuperfluousProxyHeader
	}

	// The full header was validated via Peek; consume exactly its bytes. Any
	// bytes buffered beyond this point (TLS records, application data) stay in
	// c.br and are delivered by Read.
	if _, err := c.br.Discard(size); err != nil {
		return err
	}
	c.header = header
	return nil
}

func (c *policyConn) storedReadDeadline() (time.Time, bool) {
	if v := c.readDeadline.Load(); v != nil {
		t := v.(time.Time)
		return t, !t.IsZero()
	}
	return time.Time{}, false
}
