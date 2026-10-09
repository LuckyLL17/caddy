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
	"testing"

	goproxy "github.com/pires/go-proxyproto"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestConnectionPolicyCompileDefaults(t *testing.T) {
	cp, err := (&ConnectionPolicy{}).compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !cp.allowV1 || !cp.allowV2 {
		t.Errorf("expected both versions allowed by default, got v1=%v v2=%v", cp.allowV1, cp.allowV2)
	}
	if cp.maxHeaderSize != defaultMaxHeaderSize {
		t.Errorf("expected default max header size %d, got %d", defaultMaxHeaderSize, cp.maxHeaderSize)
	}
	if cp.missing != headerPass {
		t.Errorf("expected missing header to pass by default, got %d", cp.missing)
	}
	if cp.malformed != headerFail {
		t.Errorf("expected malformed header to fail by default, got %d", cp.malformed)
	}
	if cp.remote != endpointFromHeader || cp.local != endpointFromHeader {
		t.Errorf("expected header endpoints by default, got remote=%d local=%d", cp.remote, cp.local)
	}
	if cp.tlv != nil {
		t.Errorf("expected no compiled TLV rules by default")
	}
}

func TestConnectionPolicyCompileErrors(t *testing.T) {
	for i, tc := range []struct {
		name string
		pol  ConnectionPolicy
	}{
		{"bad version", ConnectionPolicy{Versions: []int{3}}},
		{"max too small", ConnectionPolicy{MaxHeaderSize: 14}},
		{"max too large", ConnectionPolicy{MaxHeaderSize: maxConfigurableMaxHeaderSize + 1}},
		{"bad missing action", ConnectionPolicy{Missing: "nope"}},
		{"bad malformed action", ConnectionPolicy{Malformed: "nope"}},
		{"bad remote preference", ConnectionPolicy{Address: AddressPolicy{Remote: "nope"}}},
		{"bad local preference", ConnectionPolicy{Address: AddressPolicy{Local: "nope"}}},
		{"bad tlv default", ConnectionPolicy{TLV: &TLVPolicy{Default: "nope"}}},
		{"duplicate tlv type", ConnectionPolicy{TLV: &TLVPolicy{
			Accept: []TLVType{TLVType(goproxy.PP2_TYPE_ALPN)},
			Reject: []TLVType{TLVType(goproxy.PP2_TYPE_ALPN)},
		}}},
		{"noop configured", ConnectionPolicy{TLV: &TLVPolicy{
			Reject: []TLVType{TLVType(goproxy.PP2_TYPE_NOOP)},
		}}},
	} {
		if _, err := tc.pol.compile(); err == nil {
			t.Errorf("test %d (%s): expected validation error, got nil", i, tc.name)
		}
	}
}

func TestConnectionPolicyCompileTLVTable(t *testing.T) {
	pol := &ConnectionPolicy{
		TLV: &TLVPolicy{
			Default: TLVActionReject,
			Accept:  []TLVType{TLVType(goproxy.PP2_TYPE_ALPN)},
			Discard: []TLVType{TLVType(0xE0)},
		},
	}
	cp, err := pol.compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if cp.tlv.actions[goproxy.PP2_TYPE_ALPN] != tlvKeep {
		t.Errorf("expected ALPN to be kept")
	}
	if cp.tlv.actions[goproxy.PP2Type(0xE0)] != tlvDrop {
		t.Errorf("expected 0xE0 to be dropped")
	}
	if cp.tlv.actions[goproxy.PP2_TYPE_SSL] != tlvDeny {
		t.Errorf("expected unlisted SSL to be rejected by default")
	}
	if cp.tlv.actions[goproxy.PP2_TYPE_NOOP] != tlvKeep {
		t.Errorf("expected NOOP padding to always be kept")
	}
}

func TestTLVTypeUnmarshalJSON(t *testing.T) {
	for i, tc := range []struct {
		input string
		want  goproxy.PP2Type
		err   bool
	}{
		{`1`, goproxy.PP2_TYPE_ALPN, false},
		{`"ALPN"`, goproxy.PP2_TYPE_ALPN, false},
		{`"alpn"`, goproxy.PP2_TYPE_ALPN, false},
		{`"0x30"`, goproxy.PP2_TYPE_NETNS, false},
		{`"48"`, goproxy.PP2_TYPE_NETNS, false},
		{`"nope"`, 0, true},
		{`256`, 0, true},
		{`-1`, 0, true},
	} {
		var got TLVType
		err := json.Unmarshal([]byte(tc.input), &got)
		if tc.err {
			if err == nil {
				t.Errorf("test %d: expected error for %s", i, tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("test %d: unexpected error for %s: %v", i, tc.input, err)
			continue
		}
		if goproxy.PP2Type(got) != tc.want {
			t.Errorf("test %d: got %d, want %d", i, got, tc.want)
		}
	}
}

func TestListenerWrapperJSONPolicy(t *testing.T) {
	raw := `{
		"timeout": 1000000000,
		"allow": ["127.0.0.0/8"],
		"fallback_policy": "REQUIRE",
		"policy": {
			"versions": [2],
			"max_header_size": 2048,
			"missing": "reject",
			"malformed": "reject",
			"address": {"remote": "peer", "local": "header"},
			"tlv": {
				"default": "discard",
				"accept": ["ALPN", 2],
				"reject": ["SSL"]
			}
		}
	}`
	var w ListenerWrapper
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := w.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	cp := w.compiled
	if cp == nil {
		t.Fatal("expected compiled policy")
	}
	if cp.allowV1 || !cp.allowV2 {
		t.Errorf("version gate wrong: v1=%v v2=%v", cp.allowV1, cp.allowV2)
	}
	if cp.maxHeaderSize != 2048 || cp.missing != headerFail || cp.malformed != headerFail {
		t.Errorf("compiled scalars wrong: %+v", cp)
	}
	if cp.remote != endpointFromPeer || cp.local != endpointFromHeader {
		t.Errorf("address prefs wrong: remote=%d local=%d", cp.remote, cp.local)
	}
	if cp.tlv == nil || cp.tlv.actions[goproxy.PP2_TYPE_ALPN] != tlvKeep ||
		cp.tlv.actions[goproxy.PP2_TYPE_AUTHORITY] != tlvKeep ||
		cp.tlv.actions[goproxy.PP2_TYPE_NETNS] != tlvDrop ||
		cp.tlv.actions[goproxy.PP2_TYPE_SSL] != tlvDeny {
		t.Errorf("TLV action table not compiled as expected")
	}
	if len(w.allow) != 1 || w.allow[0].String() != "127.0.0.0/8" {
		t.Errorf("allow CIDR not provisioned: %+v", w.allow)
	}
}

func TestListenerWrapperJSONPolicyInvalid(t *testing.T) {
	for _, raw := range []string{
		`{"policy":{"versions":[5]}}`,
		`{"policy":{"max_header_size":4}}`,
		`{"policy":{"missing":"bogus"}}`,
		`{"policy":{"tlv":{"accept":["NOOP"]}}}`,
	} {
		var w ListenerWrapper
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			t.Errorf("unexpected unmarshal error for %s: %v", raw, err)
			continue
		}
		if err := w.Provision(caddy.Context{}); err == nil {
			t.Errorf("expected provision error for %s", raw)
		}
	}
}

func TestParseTLVTypeToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  goproxy.PP2Type
	}{
		{"SSL", goproxy.PP2_TYPE_SSL},
		{"unique_id", goproxy.PP2_TYPE_UNIQUE_ID},
		{"0x01", goproxy.PP2_TYPE_ALPN},
		{"224", goproxy.PP2Type(0xE0)},
	} {
		got, err := parseTLVTypeToken(tc.token)
		if err != nil {
			t.Errorf("token %q: unexpected error: %v", tc.token, err)
			continue
		}
		if got != tc.want {
			t.Errorf("token %q: got %d, want %d", tc.token, got, tc.want)
		}
	}
	if _, err := parseTLVTypeToken("bogus"); err == nil {
		t.Error("expected error for bogus TLV token")
	}
}

func TestListenerWrapperCaddyfilePolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{
			name: "full policy",
			ok:   true,
			body: `proxy_protocol {
				timeout 2s
				allow 127.0.0.0/8
				deny 10.0.0.0/8
				fallback_policy REQUIRE
				policy {
					versions 1 2
					max_header_size 8KiB
					missing reject
					malformed allow
					address {
						remote header
						local peer
					}
					tlv {
						default reject
						accept ALPN 0x02
						discard 0xE0
						reject SSL
					}
				}
			}`,
		},
		{
			name: "inline address",
			ok:   true,
			body: `proxy_protocol {
				policy {
					address peer
				}
			}`,
		},
		{
			name: "bad version",
			ok:   false,
			body: `proxy_protocol {
				policy {
					versions 3
				}
			}`,
		},
		{
			name: "unknown subdirective",
			ok:   false,
			body: `proxy_protocol {
				policy {
					bogus
				}
			}`,
		},
		{
			name: "bad tlv action",
			ok:   false,
			body: `proxy_protocol {
				policy {
					tlv {
						default bogus
					}
				}
			}`,
		},
		{
			name: "duplicate policy block",
			ok:   false,
			body: `proxy_protocol {
				policy {}
				policy {}
			}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := new(ListenerWrapper)
			err := w.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.body))
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected Caddyfile error: %v", err)
				}
				if err := w.Provision(caddy.Context{}); err != nil {
					t.Fatalf("provision: %v", err)
				}
				if w.compiled == nil {
					t.Fatal("expected compiled policy after provision")
				}
			} else if err == nil {
				t.Fatal("expected Caddyfile error, got nil")
			}
		})
	}
}
