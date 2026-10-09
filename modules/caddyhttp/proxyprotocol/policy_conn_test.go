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
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	goproxy "github.com/pires/go-proxyproto"

	"github.com/caddyserver/caddy/v2"
)

type testPipe struct {
	client net.Conn
	server *policyConn
}

func dialPolicyConn(t *testing.T, cp *compiledPolicy, mode goproxy.Policy, headerTimeout time.Duration) testPipe {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverConn, ok := <-accepted
	if !ok {
		t.Fatal("server never accepted the connection")
	}

	server := newPolicyConn(serverConn, cp, mode, headerTimeout)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return testPipe{client: client, server: server}
}

func mustCompile(t *testing.T, p *ConnectionPolicy) *compiledPolicy {
	t.Helper()
	cp, err := p.compile()
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	return cp
}

func mustFormat(t *testing.T, hdr *goproxy.Header) []byte {
	t.Helper()
	b, err := hdr.Format()
	if err != nil {
		t.Fatalf("format header: %v", err)
	}
	return b
}

func v2TCP4Header() *goproxy.Header {
	return &goproxy.Header{
		Version:           2,
		Command:           goproxy.PROXY,
		TransportProtocol: goproxy.TCPv4,
		SourceAddr:        &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1234},
		DestinationAddr:   &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 443},
	}
}

// readN reads exactly n bytes, failing the test on short reads.
func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

func TestPolicyConnV2AppliesHeaderAndPreservesPayload(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)

	hdr := v2TCP4Header()
	payload := []byte("POST / HTTP/1.1\r\nHost: example\r\n\r\n")
	if _, err := pipe.client.Write(append(mustFormat(t, hdr), payload...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := pipe.server.RemoteAddr().String(); got != "10.0.0.1:1234" {
		t.Errorf("remote addr: got %q, want 10.0.0.1:1234", got)
	}
	if got := pipe.server.LocalAddr().String(); got != "10.0.0.2:443" {
		t.Errorf("local addr: got %q, want 10.0.0.2:443", got)
	}
	if got := readN(t, pipe.server, len(payload)); !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %q", got)
	}
}

func TestPolicyConnV2IPv6AppliesHeader(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)

	hdr := &goproxy.Header{
		Version:           2,
		Command:           goproxy.PROXY,
		TransportProtocol: goproxy.TCPv6,
		SourceAddr:        &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1234},
		DestinationAddr:   &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 443},
	}
	payload := []byte("v6-payload")
	if _, err := pipe.client.Write(append(mustFormat(t, hdr), payload...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := pipe.server.RemoteAddr().String(); got != "[2001:db8::1]:1234" {
		t.Errorf("remote addr: got %q, want [2001:db8::1]:1234", got)
	}
	if got := pipe.server.LocalAddr().String(); got != "[2001:db8::2]:443" {
		t.Errorf("local addr: got %q, want [2001:db8::2]:443", got)
	}
	if got := readN(t, pipe.server, len(payload)); string(got) != string(payload) {
		t.Errorf("payload mismatch: got %q", got)
	}
}

func TestPolicyConnV1AppliesHeader(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)

	hdr := &goproxy.Header{
		Version:           1,
		Command:           goproxy.PROXY,
		TransportProtocol: goproxy.TCPv4,
		SourceAddr:        &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5000},
		DestinationAddr:   &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 80},
	}
	payload := []byte("raw-after-v1")
	if _, err := pipe.client.Write(append(mustFormat(t, hdr), payload...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := pipe.server.RemoteAddr().String(); got != "192.0.2.10:5000" {
		t.Errorf("remote addr: got %q, want 192.0.2.10:5000", got)
	}
	if got := readN(t, pipe.server, len(payload)); !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %q", got)
	}
}

func TestPolicyConnMissingHeader(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{Missing: HeaderActionReject})
		pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		if _, err := pipe.client.Write([]byte("GET / HTTP/1.1")); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 16)
		if _, err := pipe.server.Read(buf); !errors.Is(err, goproxy.ErrNoProxyProtocol) {
			t.Fatalf("expected ErrNoProxyProtocol, got %v", err)
		}
		// the error must be sticky on every subsequent read
		if _, err := pipe.server.Read(buf); !errors.Is(err, goproxy.ErrNoProxyProtocol) {
			t.Fatalf("expected sticky ErrNoProxyProtocol, got %v", err)
		}
	})

	t.Run("allow serves raw bytes", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{})
		pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		payload := []byte("plain-connection")
		if _, err := pipe.client.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := readN(t, pipe.server, len(payload)); !bytes.Equal(got, payload) {
			t.Errorf("payload mismatch: got %q", got)
		}
		if got, want := pipe.server.RemoteAddr().String(), pipe.client.LocalAddr().String(); got != want {
			t.Errorf("expected socket remote %q, got %q", want, got)
		}
	})
}

func TestPolicyConnMalformedHeader(t *testing.T) {
	malformed := []byte("PROXY NONSENSE\r\n")

	t.Run("reject", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{})
		pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		if _, err := pipe.client.Write(malformed); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 32)
		if _, err := pipe.server.Read(buf); err == nil {
			t.Fatal("expected malformed header error, got nil")
		}
	})

	t.Run("allow delivers raw bytes untouched", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{Malformed: HeaderActionAllow})
		pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		rest := []byte("-suffix")
		if _, err := pipe.client.Write(append(malformed, rest...)); err != nil {
			t.Fatalf("write: %v", err)
		}
		want := append(malformed, rest...)
		if got := readN(t, pipe.server, len(want)); !bytes.Equal(got, want) {
			t.Errorf("raw passthrough mismatch: got %q, want %q", got, want)
		}
	})
}

func TestPolicyConnOversizedHeader(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{MaxHeaderSize: 32})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)

	hdr := v2TCP4Header()
	if err := hdr.SetTLVs([]goproxy.TLV{
		{Type: goproxy.PP2_TYPE_ALPN, Value: bytes.Repeat([]byte("a"), 64)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.client.Write(mustFormat(t, hdr)); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 32)
	_, err := pipe.server.Read(buf)
	if !errors.Is(err, errHeaderTooLarge) {
		t.Fatalf("expected errHeaderTooLarge, got %v", err)
	}
}

func TestPolicyConnVersionGate(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{Versions: []int{1}})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
	if _, err := pipe.client.Write(mustFormat(t, v2TCP4Header())); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 32)
	if _, err := pipe.server.Read(buf); !errors.Is(err, errVersionNotAllowed) {
		t.Fatalf("expected errVersionNotAllowed, got %v", err)
	}
}

func TestPolicyConnTLV(t *testing.T) {
	t.Run("rejected type", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{TLV: &TLVPolicy{
			Reject: []TLVType{TLVType(goproxy.PP2_TYPE_SSL)},
		}})
		pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		hdr := v2TCP4Header()
		if err := hdr.SetTLVs([]goproxy.TLV{
			{Type: goproxy.PP2_TYPE_SSL, Value: []byte{0, 0, 0, 0, 0}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pipe.client.Write(mustFormat(t, hdr)); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 32)
		if _, err := pipe.server.Read(buf); !errors.Is(err, errTLVRejected) {
			t.Fatalf("expected errTLVRejected, got %v", err)
		}
	})

	t.Run("default reject with accept and discard", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{TLV: &TLVPolicy{
			Default: TLVActionReject,
			Accept:  []TLVType{TLVType(goproxy.PP2_TYPE_ALPN)},
			Discard: []TLVType{TLVType(0xE0)},
		}})

		// header with ALPN + custom 0xE0: accepted
		pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		hdr := v2TCP4Header()
		if err := hdr.SetTLVs([]goproxy.TLV{
			{Type: goproxy.PP2_TYPE_ALPN, Value: []byte("h2")},
			{Type: goproxy.PP2Type(0xE0), Value: []byte("xx")},
		}); err != nil {
			t.Fatal(err)
		}
		wire := mustFormat(t, hdr)
		if _, err := pipe.client.Write(append(wire, []byte("data")...)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := readN(t, pipe.server, 4); string(got) != "data" {
			t.Errorf("payload mismatch: got %q", got)
		}
		tlvs, err := pipe.server.header.TLVs()
		if err != nil {
			t.Fatalf("tlvs: %v", err)
		}
		if len(tlvs) != 1 || tlvs[0].Type != goproxy.PP2_TYPE_ALPN {
			t.Fatalf("expected only the accepted ALPN TLV, got %+v", tlvs)
		}

		// header with unlisted AUTHORITY: rejected
		pipe2 := dialPolicyConn(t, cp, goproxy.USE, time.Second)
		hdr2 := v2TCP4Header()
		if err := hdr2.SetTLVs([]goproxy.TLV{
			{Type: goproxy.PP2_TYPE_AUTHORITY, Value: []byte("example")},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pipe2.client.Write(mustFormat(t, hdr2)); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 16)
		if _, err := pipe2.server.Read(buf); !errors.Is(err, errTLVRejected) {
			t.Fatalf("expected errTLVRejected for default-denied TLV, got %v", err)
		}
	})
}

func TestPolicyConnLOCALUsesPeerEndpoints(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{})
	pipe := dialPolicyConn(t, cp, goproxy.REQUIRE, time.Second)

	hdr := &goproxy.Header{
		Version:           2,
		Command:           goproxy.LOCAL,
		TransportProtocol: goproxy.UNSPEC,
	}
	payload := []byte("local-conn")
	if _, err := pipe.client.Write(append(mustFormat(t, hdr), payload...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got, want := pipe.server.RemoteAddr().String(), pipe.client.LocalAddr().String(); got != want {
		t.Errorf("LOCAL header must keep socket remote addr, got %q want %q", got, want)
	}
	if got := readN(t, pipe.server, len(payload)); string(got) != string(payload) {
		t.Errorf("payload mismatch: got %q", got)
	}
}

func TestPolicyConnGatingModes(t *testing.T) {
	t.Run("IGNORE keeps socket endpoints", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{})
		pipe := dialPolicyConn(t, cp, goproxy.IGNORE, time.Second)
		if _, err := pipe.client.Write(append(mustFormat(t, v2TCP4Header()), []byte("x")...)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got, want := pipe.server.RemoteAddr().String(), pipe.client.LocalAddr().String(); got != want {
			t.Errorf("IGNORE must keep socket remote addr, got %q want %q", got, want)
		}
		if got := readN(t, pipe.server, 1); got[0] != 'x' {
			t.Errorf("payload mismatch: got %q", got)
		}
	})

	t.Run("REQUIRE rejects missing header", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{})
		pipe := dialPolicyConn(t, cp, goproxy.REQUIRE, time.Second)
		if _, err := pipe.client.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := pipe.server.Read(make([]byte, 1)); !errors.Is(err, goproxy.ErrNoProxyProtocol) {
			t.Fatalf("expected ErrNoProxyProtocol, got %v", err)
		}
	})

	t.Run("REJECT forbids header but passes raw bytes", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{})
		pipe := dialPolicyConn(t, cp, goproxy.REJECT, time.Second)
		if _, err := pipe.client.Write(mustFormat(t, v2TCP4Header())); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := pipe.server.Read(make([]byte, 16)); !errors.Is(err, goproxy.ErrSuperfluousProxyHeader) {
			t.Fatalf("expected ErrSuperfluousProxyHeader, got %v", err)
		}
	})

	t.Run("REJECT accepts headerless connection", func(t *testing.T) {
		cp := mustCompile(t, &ConnectionPolicy{})
		pipe := dialPolicyConn(t, cp, goproxy.REJECT, time.Second)
		payload := []byte("no-header")
		if _, err := pipe.client.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := readN(t, pipe.server, len(payload)); string(got) != string(payload) {
			t.Errorf("payload mismatch: got %q", got)
		}
	})
}

func TestPolicyConnAddressPreferences(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{
		Address: AddressPolicy{Remote: EndpointFromPeer, Local: EndpointFromHeader},
	})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)
	if _, err := pipe.client.Write(append(mustFormat(t, v2TCP4Header()), []byte("z")...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := pipe.server.RemoteAddr().String(), pipe.client.LocalAddr().String(); got != want {
		t.Errorf("remote should come from peer: got %q want %q", got, want)
	}
	if got := pipe.server.LocalAddr().String(); got != "10.0.0.2:443" {
		t.Errorf("local should come from header: got %q want 10.0.0.2:443", got)
	}
	if got := readN(t, pipe.server, 1); got[0] != 'z' {
		t.Errorf("payload mismatch: got %q", got)
	}
}

func TestPolicyConnChunksBufferedAcrossReads(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{})
	pipe := dialPolicyConn(t, cp, goproxy.USE, time.Second)

	const payloadLen = 300
	payload := bytes.Repeat([]byte("T"), payloadLen)
	if _, err := pipe.client.Write(append(mustFormat(t, v2TCP4Header()), payload...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, payloadLen)
	for off := 0; off < payloadLen; {
		n, err := pipe.server.Read(got[off:min(payloadLen, off+7)])
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		off += n
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("buffered payload mismatch over chunked reads")
	}
}

func TestPolicyConnHeaderTimeout(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{Missing: HeaderActionReject})
	pipe := dialPolicyConn(t, cp, goproxy.USE, 80*time.Millisecond)

	start := time.Now()
	_, err := pipe.server.Read(make([]byte, 8))
	elapsed := time.Since(start)
	if !errors.Is(err, goproxy.ErrNoProxyProtocol) {
		t.Fatalf("expected ErrNoProxyProtocol on timeout, got %v", err)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("returned before the header timeout elapsed: %v", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("header timeout did not bound the read: %v", elapsed)
	}
	// the caller's deadline must be restored, not left armed
	if err := pipe.server.SetReadDeadline(time.Time{}); err != nil {
		t.Errorf("SetReadDeadline after timed-out header: %v", err)
	}
}

func TestPolicyConnTimeoutRestoresDeadlineAndServesRawBytes(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{})
	pipe := dialPolicyConn(t, cp, goproxy.USE, 80*time.Millisecond)

	done := make(chan []byte, 1)
	errs := make(chan error, 1)
	go func() {
		buf := make([]byte, 3)
		_, err := io.ReadFull(pipe.server, buf)
		if err != nil {
			errs <- err
			return
		}
		done <- buf
	}()

	// after the header detection times out, the read must keep waiting on
	// raw stream data rather than failing
	time.Sleep(200 * time.Millisecond)
	if _, err := pipe.client.Write([]byte("abc")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-done:
		if string(got) != "abc" {
			t.Errorf("expected raw bytes after timeout, got %q", got)
		}
	case err := <-errs:
		t.Fatalf("read after timeout: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-timeout read")
	}
}

func TestPolicyConnClientCloseFailsRead(t *testing.T) {
	cp := mustCompile(t, &ConnectionPolicy{Missing: HeaderActionReject})
	pipe := dialPolicyConn(t, cp, goproxy.REQUIRE, time.Second)
	if err := pipe.client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := pipe.server.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected error when client closes before sending a header")
	}
}

func TestPolicyListenerWrappingBranch(t *testing.T) {
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawLn.Close() })

	legacy := &ListenerWrapper{}
	if err := legacy.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision legacy: %v", err)
	}
	if _, ok := legacy.WrapListener(rawLn).(*goproxy.Listener); !ok {
		t.Error("expected unconfigured wrapper to use go-proxyproto listener")
	}

	withPolicy := &ListenerWrapper{Policy: &ConnectionPolicy{}}
	if err := withPolicy.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision policy: %v", err)
	}
	if _, ok := withPolicy.WrapListener(rawLn).(*policyListener); !ok {
		t.Error("expected configured wrapper to use policyListener")
	}
}

// provisionedWrapper builds and provisions a listener wrapper for listener
// level tests.
func provisionedWrapper(t *testing.T, w *ListenerWrapper) *ListenerWrapper {
	t.Helper()
	if err := w.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	return w
}

func TestPolicyListenerSKIPBypassesHeader(t *testing.T) {
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawLn.Close() })

	w := provisionedWrapper(t, &ListenerWrapper{
		FallbackPolicy: PolicySKIP,
		Policy:         &ConnectionPolicy{},
	})
	ln := w.WrapListener(rawLn)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp", rawLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	conn := <-accepted
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Fatalf("SKIP must return the raw connection, got %T", conn)
	}
	t.Cleanup(func() { _ = conn.Close() })

	wire := append(mustFormat(t, v2TCP4Header()), []byte("raw")...)
	if _, err := client.Write(wire); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readN(t, conn, len(wire)); !bytes.Equal(got, wire) {
		t.Errorf("SKIP must not consume PROXY bytes; got %q…", got[:min(16, len(got))])
	}
}

func TestPolicyListenerDenyREJECT(t *testing.T) {
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawLn.Close() })

	w := provisionedWrapper(t, &ListenerWrapper{
		Deny:   []string{"127.0.0.0/8"},
		Policy: &ConnectionPolicy{},
	})
	ln := w.WrapListener(rawLn)

	acceptOne := func() net.Conn {
		c, err := ln.Accept()
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	client, err := net.Dial("tcp", rawLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	conn := acceptOne()
	if _, err := client.Write(mustFormat(t, v2TCP4Header())); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := conn.Read(make([]byte, 16)); !errors.Is(err, goproxy.ErrSuperfluousProxyHeader) {
		t.Fatalf("denied source sending a header: expected ErrSuperfluousProxyHeader, got %v", err)
	}

	client2, err := net.Dial("tcp", rawLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client2.Close() })
	conn2 := acceptOne()
	payload := []byte("plain")
	if _, err := client2.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readN(t, conn2, len(payload)); string(got) != string(payload) {
		t.Errorf("denied source without header should be served raw, got %q", got)
	}
}

// TestPolicyListenerConcurrent exercises many simultaneous connections to
// surface shared-state races under -race.
func TestPolicyListenerConcurrent(t *testing.T) {
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawLn.Close() })

	w := provisionedWrapper(t, &ListenerWrapper{
		Allow: []string{"127.0.0.0/8"},
		Policy: &ConnectionPolicy{TLV: &TLVPolicy{
			Default: TLVActionReject,
			Accept:  []TLVType{TLVType(goproxy.PP2_TYPE_ALPN)},
		}},
	})
	ln := w.WrapListener(rawLn)

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := net.Dial("tcp", rawLn.Addr().String())
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer client.Close()

			conn, err := ln.Accept()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			defer conn.Close()

			hdr := v2TCP4Header()
			if err := hdr.SetTLVs([]goproxy.TLV{
				{Type: goproxy.PP2_TYPE_ALPN, Value: []byte("h2")},
			}); err != nil {
				t.Errorf("set tlvs: %v", err)
				return
			}
			if _, err := client.Write(append(mustFormat(t, hdr), []byte("ok")...)); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			buf := make([]byte, 2)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if string(buf) != "ok" {
				t.Errorf("unexpected payload %q", buf)
			}
			if addr := conn.RemoteAddr().String(); addr != "10.0.0.1:1234" {
				t.Errorf("remote addr: %s", addr)
			}
		}()
	}
	wg.Wait()
}
