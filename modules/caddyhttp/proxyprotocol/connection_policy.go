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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	goproxy "github.com/pires/go-proxyproto"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// HeaderAction decides how a connection proceeds when its PROXY header is
// missing or malformed.
type HeaderAction string

const (
	// HeaderActionAllow delivers the connection's bytes untouched: a missing
	// header makes the connection behave as a plain byte stream, and a
	// malformed header leaves the received bytes available as raw stream data.
	HeaderActionAllow HeaderAction = "allow"

	// HeaderActionReject fails the first read from or write to the connection
	// with a PROXY protocol error. The connection itself is not closed by the
	// wrapper; the layer above is expected to close it.
	HeaderActionReject HeaderAction = "reject"
)

// EndpointPreference selects which endpoints a validated connection reports.
type EndpointPreference string

const (
	// EndpointFromHeader reports the endpoints carried in the PROXY header.
	// LOCAL-command headers always report the real socket endpoints per the
	// protocol specification.
	EndpointFromHeader EndpointPreference = "header"

	// EndpointFromPeer always reports the real socket peer (equivalent to the
	// IGNORE policy for the reported addresses).
	EndpointFromPeer EndpointPreference = "peer"
)

// TLVAction decides what a v2 TLV rule does with a TLV type.
type TLVAction string

const (
	// TLVActionAccept keeps the TLV in the parsed, connection-scoped header.
	TLVActionAccept TLVAction = "accept"

	// TLVActionDiscard tolerates the TLV on the wire but drops it from the
	// parsed header. TLVs never remain in the byte stream handed to upper
	// layers regardless of this setting.
	TLVActionDiscard TLVAction = "discard"

	// TLVActionReject treats a header carrying the TLV type as invalid.
	TLVActionReject TLVAction = "reject"
)

const (
	// defaultMaxHeaderSize bounds the total on-wire header size (signature,
	// address block and TLVs) accepted when no size is configured.
	defaultMaxHeaderSize = 4096

	// minConfigurableMaxHeaderSize is the shortest possible header:
	// "PROXY UNKNOWN\r\n".
	minConfigurableMaxHeaderSize = 15

	// maxConfigurableMaxHeaderSize is the 16-byte v2 prefix plus the largest
	// 16-bit length field.
	maxConfigurableMaxHeaderSize = 16 + math.MaxUint16
)

// ConnectionPolicy is an optional, deterministic policy for PROXY protocol
// connections. It is validated and compiled into a read-only form while the
// configuration is provisioned; accepted connections then evaluate it with
// connection-local state only.
//
// When set, source gating still follows the wrapper's Allow, Deny and
// FallbackPolicy settings: denied sources are refused, SKIP bypasses header
// processing, REQUIRE mandates a header, REJECT forbids one, and IGNORE keeps
// the socket addresses. The policy below additionally constrains every
// header that is actually parsed.
type ConnectionPolicy struct {
	// Versions is the list of accepted PROXY protocol versions.
	// Valid entries are 1 and 2. Default: both versions.
	Versions []int `json:"versions,omitempty"`

	// MaxHeaderSize bounds the total on-wire PROXY header size in bytes,
	// including the signature, address block and TLVs. Headers larger than
	// this are treated as malformed.
	//
	// Default: 4096. Minimum: 15. Maximum: 65551.
	MaxHeaderSize int `json:"max_header_size,omitempty"`

	// Missing decides what happens when no PROXY header is present.
	// Default: allow (the raw connection is served).
	Missing HeaderAction `json:"missing,omitempty"`

	// Malformed decides what happens when a PROXY header signature is
	// present but the header violates the policy or the specification.
	// Default: reject.
	Malformed HeaderAction `json:"malformed,omitempty"`

	// Address selects which endpoints the connection reports once a valid
	// PROXY-command header has been parsed.
	Address AddressPolicy `json:"address,omitzero"`

	// TLV constrains the type-length-value entries of v2 headers. When nil,
	// TLV contents are neither inspected nor retained.
	TLV *TLVPolicy `json:"tlv,omitempty"`
}

// AddressPolicy selects the source of the reported remote and local
// connection endpoints.
type AddressPolicy struct {
	// Remote selects the reported source (client) endpoint.
	// Default: header.
	Remote EndpointPreference `json:"remote,omitempty"`

	// Local selects the reported destination (server) endpoint.
	// Default: header.
	Local EndpointPreference `json:"local,omitempty"`
}

// TLVPolicy is a deterministic rule set for PROXY protocol v2 TLVs. Each TLV
// type is matched at most once: types listed in Accept, Discard and Reject
// take their explicit action, every other type takes the Default action.
// NOOP padding (type 0x04) is always accepted and stripped.
type TLVPolicy struct {
	// Default is the action for TLV types not listed in Accept, Discard or
	// Reject. Default: accept.
	Default TLVAction `json:"default,omitempty"`

	// Accept lists TLV types retained in the parsed connection header.
	Accept []TLVType `json:"accept,omitempty"`

	// Discard lists TLV types tolerated on the wire but not retained.
	Discard []TLVType `json:"discard,omitempty"`

	// Reject lists TLV types that invalidate the carrying header.
	Reject []TLVType `json:"reject,omitempty"`
}

// TLVType identifies a PROXY protocol v2 TLV type. JSON accepts either a
// number between 0 and 255 or a symbolic name: ALPN, AUTHORITY, CRC32C,
// NOOP, UNIQUE_ID, SSL, NETNS.
type TLVType int

// UnmarshalJSON accepts a numeric TLV type or a symbolic name.
func (t *TLVType) UnmarshalJSON(data []byte) error {
	var num int
	if err := json.Unmarshal(data, &num); err == nil {
		if num < 0 || num > math.MaxUint8 {
			return fmt.Errorf("proxy_protocol: TLV type %d out of range (0-255)", num)
		}
		*t = TLVType(num)
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("proxy_protocol: TLV type must be a number or a symbolic name: %v", err)
	}
	typ, err := parseTLVTypeToken(name)
	if err != nil {
		return err
	}
	*t = TLVType(typ)
	return nil
}

// compiledPolicy is the immutable, provisioned form of ConnectionPolicy. It
// is shared read-only across connections and never mutated after provisioning.
type compiledPolicy struct {
	allowV1       bool
	allowV2       bool
	maxHeaderSize int
	missing       headerDisposition
	malformed     headerDisposition
	remote        endpointSource
	local         endpointSource
	tlv           *compiledTLV
}

type headerDisposition uint8

const (
	headerPass headerDisposition = iota // serve the connection, untouched bytes stay available
	headerFail                          // fail the connection with a PROXY protocol error
)

type endpointSource uint8

const (
	endpointFromHeader endpointSource = iota
	endpointFromPeer
)

type tlvDisposition uint8

const (
	tlvKeep tlvDisposition = iota
	tlvDrop
	tlvDeny
)

// compiledTLV is a flat, allocation-free per-type action table.
type compiledTLV struct {
	actions [256]tlvDisposition
}

// compile validates the policy and returns its read-only compiled form.
func (p *ConnectionPolicy) compile() (*compiledPolicy, error) {
	cp := &compiledPolicy{
		maxHeaderSize: defaultMaxHeaderSize,
		missing:       headerPass,
		malformed:     headerFail,
		remote:        endpointFromHeader,
		local:         endpointFromHeader,
	}

	if len(p.Versions) > 0 {
		for _, v := range p.Versions {
			switch v {
			case 1:
				cp.allowV1 = true
			case 2:
				cp.allowV2 = true
			default:
				return nil, fmt.Errorf("proxy_protocol: unsupported PROXY protocol version %d in policy (only 1 and 2 are supported)", v)
			}
		}
	} else {
		cp.allowV1 = true
		cp.allowV2 = true
	}

	if p.MaxHeaderSize != 0 {
		if p.MaxHeaderSize < minConfigurableMaxHeaderSize || p.MaxHeaderSize > maxConfigurableMaxHeaderSize {
			return nil, fmt.Errorf("proxy_protocol: max_header_size must be between %d and %d bytes, got %d",
				minConfigurableMaxHeaderSize, maxConfigurableMaxHeaderSize, p.MaxHeaderSize)
		}
		cp.maxHeaderSize = p.MaxHeaderSize
	}

	switch p.Missing {
	case "", HeaderActionAllow:
	case HeaderActionReject:
		cp.missing = headerFail
	default:
		return nil, fmt.Errorf("proxy_protocol: invalid missing header action %q (allowed: %s, %s)",
			p.Missing, HeaderActionAllow, HeaderActionReject)
	}

	switch p.Malformed {
	case "", HeaderActionReject:
	case HeaderActionAllow:
		cp.malformed = headerPass
	default:
		return nil, fmt.Errorf("proxy_protocol: invalid malformed header action %q (allowed: %s, %s)",
			p.Malformed, HeaderActionAllow, HeaderActionReject)
	}

	var err error
	if cp.remote, err = compileEndpointPreference("address remote", p.Address.Remote); err != nil {
		return nil, err
	}
	if cp.local, err = compileEndpointPreference("address local", p.Address.Local); err != nil {
		return nil, err
	}

	if p.TLV != nil {
		ct, err := p.TLV.compile()
		if err != nil {
			return nil, err
		}
		cp.tlv = ct
	}

	return cp, nil
}

func compileEndpointPreference(field string, pref EndpointPreference) (endpointSource, error) {
	switch pref {
	case "", EndpointFromHeader:
		return endpointFromHeader, nil
	case EndpointFromPeer:
		return endpointFromPeer, nil
	default:
		return 0, fmt.Errorf("proxy_protocol: invalid %s preference %q (allowed: %s, %s)",
			field, pref, EndpointFromHeader, EndpointFromPeer)
	}
}

// compile validates the TLV rules and fills the read-only action table.
func (p *TLVPolicy) compile() (*compiledTLV, error) {
	ct := new(compiledTLV)

	def := tlvKeep
	switch p.Default {
	case "", TLVActionAccept:
	case TLVActionDiscard:
		def = tlvDrop
	case TLVActionReject:
		def = tlvDeny
	default:
		return nil, fmt.Errorf("proxy_protocol: invalid tlv default action %q (allowed: %s, %s, %s)",
			p.Default, TLVActionAccept, TLVActionDiscard, TLVActionReject)
	}
	for i := range ct.actions {
		ct.actions[i] = def
	}
	// NOOP padding is never policy-relevant: it is accepted and stripped.
	ct.actions[goproxy.PP2_TYPE_NOOP] = tlvKeep

	configured := make(map[goproxy.PP2Type]string)
	apply := func(types []TLVType, action tlvDisposition, actionName string) error {
		for _, raw := range types {
			typ := goproxy.PP2Type(raw)
			if typ == goproxy.PP2_TYPE_NOOP {
				return errors.New("proxy_protocol: tlv type NOOP (4) is padding and cannot be configured")
			}
			if prior, ok := configured[typ]; ok {
				return fmt.Errorf("proxy_protocol: tlv type %d is configured as both %q and %q", typ, prior, actionName)
			}
			configured[typ] = actionName
			ct.actions[typ] = action
		}
		return nil
	}
	if err := apply(p.Accept, tlvKeep, string(TLVActionAccept)); err != nil {
		return nil, err
	}
	if err := apply(p.Discard, tlvDrop, string(TLVActionDiscard)); err != nil {
		return nil, err
	}
	if err := apply(p.Reject, tlvDeny, string(TLVActionReject)); err != nil {
		return nil, err
	}
	return ct, nil
}

var tlvTypeNames = map[string]goproxy.PP2Type{
	"ALPN":      goproxy.PP2_TYPE_ALPN,
	"AUTHORITY": goproxy.PP2_TYPE_AUTHORITY,
	"CRC32C":    goproxy.PP2_TYPE_CRC32C,
	"NOOP":      goproxy.PP2_TYPE_NOOP,
	"UNIQUE_ID": goproxy.PP2_TYPE_UNIQUE_ID,
	"SSL":       goproxy.PP2_TYPE_SSL,
	"NETNS":     goproxy.PP2_TYPE_NETNS,
}

// parseTLVTypeToken resolves a symbolic TLV name, a hexadecimal (0x-prefixed)
// or a decimal TLV type number.
func parseTLVTypeToken(token string) (goproxy.PP2Type, error) {
	if typ, ok := tlvTypeNames[strings.ToUpper(strings.TrimSpace(token))]; ok {
		return typ, nil
	}
	base := 10
	num := token
	if strings.HasPrefix(token, "0x") || strings.HasPrefix(token, "0X") {
		base = 16
		num = token[2:]
	}
	n, err := strconv.ParseUint(num, base, 8)
	if err != nil {
		return 0, fmt.Errorf("proxy_protocol: invalid TLV type %q: %v", token, err)
	}
	return goproxy.PP2Type(n), nil
}

// unmarshalCaddyfile parses the policy block. Syntax:
//
//	policy {
//		versions <1|2...>
//		max_header_size <size>
//		missing <allow|reject>
//		malformed <allow|reject>
//		address <header|peer>
//		address {
//			remote <header|peer>
//			local <header|peer>
//		}
//		tlv {
//			default <accept|discard|reject>
//			accept <types...>
//			discard <types...>
//			reject <types...>
//		}
//	}
func (p *ConnectionPolicy) unmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "versions":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			for _, arg := range args {
				v, err := strconv.Atoi(arg)
				if err != nil || (v != 1 && v != 2) {
					return d.Errf("proxy_protocol policy version must be 1 and/or 2, got %q", arg)
				}
				p.Versions = append(p.Versions, v)
			}

		case "max_header_size":
			if !d.NextArg() {
				return d.ArgErr()
			}
			size, err := parseCaddyfileSize(d.Val())
			if err != nil {
				return d.Errf("invalid proxy_protocol max_header_size %q: %v", d.Val(), err)
			}
			p.MaxHeaderSize = size
			if d.NextArg() {
				return d.ArgErr()
			}

		case "missing":
			action, err := nextHeaderAction(d)
			if err != nil {
				return err
			}
			p.Missing = action

		case "malformed":
			action, err := nextHeaderAction(d)
			if err != nil {
				return err
			}
			p.Malformed = action

		case "address":
			if d.NextArg() {
				pref, err := parseEndpointToken(d.Val())
				if err != nil {
					return d.WrapErr(err)
				}
				if d.NextArg() {
					return d.ArgErr()
				}
				p.Address.Remote = pref
				p.Address.Local = pref
				break
			}
			for nesting := d.Nesting(); d.NextBlock(nesting); {
				var target *EndpointPreference
				switch d.Val() {
				case "remote":
					target = &p.Address.Remote
				case "local":
					target = &p.Address.Local
				default:
					return d.Errf("proxy_protocol address subdirective must be remote or local, got %q", d.Val())
				}
				if !d.NextArg() || d.NextArg() {
					return d.ArgErr()
				}
				pref, err := parseEndpointToken(d.Val())
				if err != nil {
					return d.WrapErr(err)
				}
				*target = pref
			}

		case "tlv":
			if d.NextArg() {
				return d.ArgErr()
			}
			tlvPolicy := new(TLVPolicy)
			for nesting := d.Nesting(); d.NextBlock(nesting); {
				switch d.Val() {
				case "default":
					if !d.NextArg() || d.NextArg() {
						return d.ArgErr()
					}
					switch TLVAction(d.Val()) {
					case TLVActionAccept, TLVActionDiscard, TLVActionReject:
						tlvPolicy.Default = TLVAction(d.Val())
					default:
						return d.Errf("proxy_protocol tlv default action must be %s, %s or %s, got %q",
							TLVActionAccept, TLVActionDiscard, TLVActionReject, d.Val())
					}
				case "accept", "discard", "reject":
					actionName := d.Val()
					args := d.RemainingArgs()
					if len(args) == 0 {
						return d.ArgErr()
					}
					for _, arg := range args {
						typ, err := parseTLVTypeToken(arg)
						if err != nil {
							return d.WrapErr(err)
						}
						switch actionName {
						case "accept":
							tlvPolicy.Accept = append(tlvPolicy.Accept, TLVType(typ))
						case "discard":
							tlvPolicy.Discard = append(tlvPolicy.Discard, TLVType(typ))
						case "reject":
							tlvPolicy.Reject = append(tlvPolicy.Reject, TLVType(typ))
						}
					}
				default:
					return d.Errf("proxy_protocol tlv subdirective must be default, accept, discard or reject, got %q", d.Val())
				}
			}
			p.TLV = tlvPolicy

		default:
			return d.ArgErr()
		}
	}
	return nil
}

// parseCaddyfileSize parses a human-readable byte size (e.g. "4KiB", "1MB").
func parseCaddyfileSize(s string) (int, error) {
	n, err := humanize.ParseBytes(s)
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt {
		return 0, fmt.Errorf("size %s is too large", s)
	}
	return int(n), nil
}

func nextHeaderAction(d *caddyfile.Dispenser) (HeaderAction, error) {
	if !d.NextArg() || d.NextArg() {
		return "", d.ArgErr()
	}
	switch HeaderAction(d.Val()) {
	case HeaderActionAllow:
		return HeaderActionAllow, nil
	case HeaderActionReject:
		return HeaderActionReject, nil
	default:
		return "", d.Errf("proxy_protocol header action must be %s or %s, got %q",
			HeaderActionAllow, HeaderActionReject, d.Val())
	}
}

func parseEndpointToken(token string) (EndpointPreference, error) {
	switch EndpointPreference(token) {
	case EndpointFromHeader:
		return EndpointFromHeader, nil
	case EndpointFromPeer:
		return EndpointFromPeer, nil
	default:
		return "", fmt.Errorf("proxy_protocol address preference must be %q or %q, got %q",
			EndpointFromHeader, EndpointFromPeer, token)
	}
}
