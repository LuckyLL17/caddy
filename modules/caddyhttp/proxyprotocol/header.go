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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	goproxy "github.com/pires/go-proxyproto"
)

const (
	// v1MaxHeaderLen is the specification maximum for a text header, CRLF
	// included.
	v1MaxHeaderLen = 107

	// v2FixedLen is the fixed binary prefix: 12-byte signature, version and
	// command byte, family byte and 2-byte length.
	v2FixedLen = 16
)

var (
	errVersionNotAllowed = errors.New("proxy_protocol: PROXY protocol version not allowed by policy")
	errHeaderTooLarge    = errors.New("proxy_protocol: PROXY protocol header exceeds configured max size")
	errTLVRejected       = errors.New("proxy_protocol: PROXY protocol TLV rejected by policy")
)

// peekHeader locates and validates a PROXY protocol header using Peek only:
// no byte is consumed on any outcome, so a tolerated malformed or missing
// header leaves the whole byte stream available to upper layers.
//
// Semantics:
//   - present==false, timedOut==true, err==nil: the read timed out
//   - present==false, timedOut==false, err==nil: no header was detected
//   - present==false, err!=nil: the stream failed before a signature was seen
//   - present==true,  err==nil: a policy-valid header and its on-wire size
//   - present==true,  err!=nil: a signature was seen but the header is invalid
func peekHeader(br *bufio.Reader, cp *compiledPolicy) (hdr *goproxy.Header, size int, present, timedOut bool, err error) {
	first, err := br.Peek(1)
	if err != nil {
		if isTimeout(err) {
			return nil, 0, false, true, nil
		}
		if errors.Is(err, io.EOF) {
			return nil, 0, false, false, nil
		}
		return nil, 0, false, false, err
	}
	if !bytes.Equal(first, goproxy.SIGV1[:1]) && !bytes.Equal(first, goproxy.SIGV2[:1]) {
		return nil, 0, false, false, nil
	}

	signature, err := br.Peek(5)
	if err != nil {
		if isTimeout(err) {
			return nil, 0, false, true, nil
		}
		if errors.Is(err, io.EOF) {
			return nil, 0, false, false, nil
		}
		return nil, 0, false, false, err
	}
	if bytes.Equal(signature, goproxy.SIGV1) {
		return peekV1(br, cp)
	}

	signature, err = br.Peek(12)
	if err != nil {
		if isTimeout(err) {
			return nil, 0, false, true, nil
		}
		if errors.Is(err, io.EOF) {
			return nil, 0, false, false, nil
		}
		return nil, 0, false, false, err
	}
	if !bytes.Equal(signature, goproxy.SIGV2) {
		return nil, 0, false, false, nil
	}
	return peekV2(br, cp)
}

// peekV1 progressively peeks until the v1 line terminator is found. Peeking
// incrementally (rather than peeking the maximum size up front) avoids
// blocking on protocols where the server speaks first.
func peekV1(br *bufio.Reader, cp *compiledPolicy) (*goproxy.Header, int, bool, bool, error) {
	if !cp.allowV1 {
		return nil, 0, true, false, errVersionNotAllowed
	}
	limit := min(cp.maxHeaderSize, v1MaxHeaderLen)
	var line []byte
	for n := 8; n <= limit; n++ {
		b, err := br.Peek(n)
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line = b[:i+1]
			break
		}
		if err != nil {
			if isTimeout(err) {
				return nil, 0, false, true, nil
			}
			return nil, 0, true, false, fmt.Errorf("%w: %v", goproxy.ErrCantReadVersion1Header, err)
		}
	}
	if line == nil {
		return nil, 0, true, false, goproxy.ErrVersion1HeaderTooLong
	}
	hdr, err := parseV1(line)
	if err != nil {
		return nil, 0, true, false, err
	}
	return hdr, len(line), true, false, nil
}

// parseV1 parses a complete v1 header line including its trailing CRLF.
func parseV1(line []byte) (*goproxy.Header, error) {
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, goproxy.ErrLineMustEndWithCrlf
	}

	tokens := strings.Split(string(line[:len(line)-2]), " ")
	if tokens[0] != "PROXY" {
		return nil, goproxy.ErrCantReadVersion1Header
	}
	if len(tokens) < 2 {
		return nil, goproxy.ErrCantReadAddressFamilyAndProtocol
	}

	hdr := &goproxy.Header{Version: 1, Command: goproxy.PROXY}
	switch tokens[1] {
	case "TCP4":
		hdr.TransportProtocol = goproxy.TCPv4
	case "TCP6":
		hdr.TransportProtocol = goproxy.TCPv6
	case "UNKNOWN":
		// UNKNOWN carries no addresses; represent it as a LOCAL command.
		hdr.TransportProtocol = goproxy.UNSPEC
		hdr.Command = goproxy.LOCAL
		return hdr, nil
	default:
		return nil, goproxy.ErrCantReadAddressFamilyAndProtocol
	}
	if len(tokens) != 6 {
		return nil, goproxy.ErrCantReadAddressFamilyAndProtocol
	}

	src, err := parseV1IP(hdr.TransportProtocol, tokens[2])
	if err != nil {
		return nil, err
	}
	dst, err := parseV1IP(hdr.TransportProtocol, tokens[3])
	if err != nil {
		return nil, err
	}
	srcPort, err := parseV1Port(tokens[4])
	if err != nil {
		return nil, err
	}
	dstPort, err := parseV1Port(tokens[5])
	if err != nil {
		return nil, err
	}
	hdr.SourceAddr = &net.TCPAddr{IP: src, Port: srcPort}
	hdr.DestinationAddr = &net.TCPAddr{IP: dst, Port: dstPort}
	return hdr, nil
}

func parseV1Port(s string) (int, error) {
	if len(s) > 1 && s[0] == '0' {
		return 0, goproxy.ErrInvalidPortNumber
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", goproxy.ErrInvalidPortNumber, err)
	}
	return int(port), nil
}

func parseV1IP(fam goproxy.AddressFamilyAndProtocol, s string) (net.IP, error) {
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.Zone() != "" {
		return nil, fmt.Errorf("%w: %s", goproxy.ErrInvalidAddress, s)
	}
	switch fam {
	case goproxy.TCPv4:
		if addr.Is4() {
			return net.IP(slices.Clone(addr.AsSlice())), nil
		}
	case goproxy.TCPv6:
		if addr.Is6() || addr.Is4In6() {
			return net.IP(slices.Clone(addr.AsSlice())), nil
		}
	case goproxy.UNSPEC, goproxy.UDPv4, goproxy.UDPv6, goproxy.UnixStream, goproxy.UnixDatagram:
		return nil, fmt.Errorf("%w: %s", goproxy.ErrInvalidAddress, s)
	default:
		return nil, fmt.Errorf("%w: %s", goproxy.ErrInvalidAddress, s)
	}
	return nil, fmt.Errorf("%w: %s", goproxy.ErrInvalidAddress, s)
}

// peekV2 peeks and validates a complete binary v2 header.
func peekV2(br *bufio.Reader, cp *compiledPolicy) (*goproxy.Header, int, bool, bool, error) {
	if !cp.allowV2 {
		return nil, 0, true, false, errVersionNotAllowed
	}

	prefix, err := br.Peek(v2FixedLen)
	if err != nil {
		if isTimeout(err) {
			return nil, 0, false, true, nil
		}
		return nil, 0, true, false, fmt.Errorf("%w: %v", goproxy.ErrCantReadProtocolVersionAndCommand, err)
	}

	command := goproxy.ProtocolVersionAndCommand(prefix[12])
	if command != goproxy.LOCAL && command != goproxy.PROXY {
		return nil, 0, true, false, goproxy.ErrUnsupportedProtocolVersionAndCommand
	}
	family := goproxy.AddressFamilyAndProtocol(prefix[13])
	switch family {
	case goproxy.UNSPEC, goproxy.TCPv4, goproxy.UDPv4, goproxy.TCPv6, goproxy.UDPv6, goproxy.UnixStream, goproxy.UnixDatagram:
	default:
		return nil, 0, true, false, goproxy.ErrUnsupportedAddressFamilyAndProtocol
	}
	if family == goproxy.UNSPEC && command == goproxy.PROXY {
		return nil, 0, true, false, goproxy.ErrUnsupportedAddressFamilyAndProtocol
	}

	length := int(binary.BigEndian.Uint16(prefix[14:16]))
	total := v2FixedLen + length
	if total > cp.maxHeaderSize {
		return nil, 0, true, false, fmt.Errorf("%w: %d bytes", errHeaderTooLarge, total)
	}

	full, err := br.Peek(total)
	if err != nil {
		if isTimeout(err) {
			return nil, 0, false, true, nil
		}
		return nil, 0, true, false, fmt.Errorf("%w: %v", goproxy.ErrCantReadLength, err)
	}

	// LOCAL headers carry connection-management information the receiver must
	// ignore per the specification; the whole frame is simply consumed.
	if command == goproxy.LOCAL {
		return &goproxy.Header{
			Version:           2,
			Command:           goproxy.LOCAL,
			TransportProtocol: goproxy.UNSPEC,
		}, total, true, false, nil
	}

	block, ok := v2AddressBlockSize(family)
	if !ok || length < block {
		return nil, 0, true, false, goproxy.ErrInvalidLength
	}

	hdr := &goproxy.Header{
		Version:           2,
		Command:           command,
		TransportProtocol: family,
	}
	body := full[v2FixedLen:]
	if err := parseV2Addresses(hdr, family, body[:block]); err != nil {
		return nil, 0, true, false, err
	}

	if cp.tlv != nil {
		tlvs, err := goproxy.SplitTLVs(body[block:])
		if err != nil {
			return nil, 0, true, false, err
		}
		retained := make([]goproxy.TLV, 0, len(tlvs))
		for _, tlv := range tlvs {
			switch cp.tlv.actions[tlv.Type] {
			case tlvKeep:
				if tlv.Type == goproxy.PP2_TYPE_NOOP {
					continue
				}
				retained = append(retained, tlv)
			case tlvDrop:
			case tlvDeny:
				return nil, 0, true, false, fmt.Errorf("%w: type 0x%02x", errTLVRejected, tlv.Type)
			}
		}
		if err := hdr.SetTLVs(retained); err != nil {
			return nil, 0, true, false, err
		}
	}

	return hdr, total, true, false, nil
}

// v2AddressBlockSize maps a transport family to its fixed address block size.
func v2AddressBlockSize(fam goproxy.AddressFamilyAndProtocol) (int, bool) {
	switch fam {
	case goproxy.TCPv4, goproxy.UDPv4:
		return 12, true
	case goproxy.TCPv6, goproxy.UDPv6:
		return 36, true
	case goproxy.UnixStream, goproxy.UnixDatagram:
		return 216, true
	case goproxy.UNSPEC:
		return 0, false
	default:
		return 0, false
	}
}

func parseV2Addresses(hdr *goproxy.Header, fam goproxy.AddressFamilyAndProtocol, b []byte) error {
	switch fam {
	case goproxy.TCPv4, goproxy.UDPv4, goproxy.TCPv6, goproxy.UDPv6:
		addrLen := 4
		if fam.IsIPv6() {
			addrLen = 16
		}
		src := net.IP(slices.Clone(b[0:addrLen]))
		dst := net.IP(slices.Clone(b[addrLen : 2*addrLen]))
		srcPort := int(binary.BigEndian.Uint16(b[2*addrLen : 2*addrLen+2]))
		dstPort := int(binary.BigEndian.Uint16(b[2*addrLen+2 : 2*addrLen+4]))
		if fam.IsStream() {
			hdr.SourceAddr = &net.TCPAddr{IP: src, Port: srcPort}
			hdr.DestinationAddr = &net.TCPAddr{IP: dst, Port: dstPort}
		} else {
			hdr.SourceAddr = &net.UDPAddr{IP: src, Port: srcPort}
			hdr.DestinationAddr = &net.UDPAddr{IP: dst, Port: dstPort}
		}

	case goproxy.UnixStream, goproxy.UnixDatagram:
		network := "unix"
		if fam.IsDatagram() {
			network = "unixgram"
		}
		hdr.SourceAddr = &net.UnixAddr{Net: network, Name: parseUnixName(b[0:108])}
		hdr.DestinationAddr = &net.UnixAddr{Net: network, Name: parseUnixName(b[108:216])}

	case goproxy.UNSPEC:
		return goproxy.ErrUnsupportedAddressFamilyAndProtocol
	default:
		return goproxy.ErrUnsupportedAddressFamilyAndProtocol
	}
	return nil
}

func parseUnixName(b []byte) string {
	if before, _, ok := bytes.Cut(b, []byte{0}); ok {
		return string(before)
	}
	return string(b)
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
