package integration

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
)

func TestBrowse(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		file_server browse
	}
  `, "caddyfile")

	req, err := http.NewRequest(http.MethodGet, "http://localhost:9080/", nil)
	if err != nil {
		t.Fail()
		return
	}
	tester.AssertResponseCode(req, 200)
}

func TestRespondWithJSON(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	localhost {
		respond {http.request.body}
	}
  `, "caddyfile")

	res, _ := tester.AssertPostResponseBody("https://localhost:9443/",
		nil,
		bytes.NewBufferString(`{
		"greeting": "Hello, world!"
	}`), 200, `{
		"greeting": "Hello, world!"
	}`)
	if res.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type to be application/json, but was %s", res.Header.Get("Content-Type"))
	}
}

func TestRequestBodyPlaceholderRespectsMaxSizeInTemplate(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			max_size 10
		}
		respond "{{placeholder \"http.request.body\"}}"
		templates
	}
  `, "caddyfile")

	req, err := http.NewRequest(http.MethodPost, "http://localhost:9080/", bytes.NewBufferString("abcdefghijklm"))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	res := tester.AssertResponseCode(req, http.StatusRequestEntityTooLarge)
	res.Body.Close()
}

func TestRequestBodyPlaceholderRespectsMaxSizeInVarsRegexp(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			max_size 10
		}
		@bigbody {
			vars_regexp {http.request.body} "^.{20}"
		}
		handle @bigbody {
			respond "the body was too long matched"
		}
		respond "no match"
	}
  `, "caddyfile")

	req, err := http.NewRequest(http.MethodPost, "http://localhost:9080/", bytes.NewBufferString("abcdefghijklmnopqrstuvwxyz"))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	res := tester.AssertResponseCode(req, http.StatusRequestEntityTooLarge)
	res.Body.Close()
}

func TestRequestBodyPlaceholderDirectRespondExpansion(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			max_size 10
		}
		respond "{http.request.body}"
	}
  `, "caddyfile")

	// direct placeholder consumers expand the sanitized request-body limit
	// marker to its text instead of failing the request with 413; only the
	// templates and vars matchers propagate the 413 status
	tester.AssertPostResponseBody("http://localhost:9080/", nil, bytes.NewBufferString("abcdefghijklm"), http.StatusOK, "request body: http: request body too large")
}

// TestRequestBodyReplayErrorRoute verifies that a body fully consumed
// in the primary route (here by the vars_regexp matcher) can be read
// again in handle_errors when the replay policy allows error routes,
// including a body large enough to spill from memory onto disk.
func TestRequestBodyReplayErrorRoute(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			replay {
				memory 64
				max_size 1MB
				allow error_routes
			}
		}
		@hasbody vars_regexp {http.request.body} .
		handle @hasbody {
			error 503
		}
		handle_errors {
			respond "{http.request.body}" 200
		}
	}
  `, "caddyfile")

	body := bytes.Repeat([]byte("replay-on-error-"), 200)
	tester.AssertPostResponseBody("http://localhost:9080/", nil, bytes.NewBuffer(body), http.StatusOK, string(body))
}

// TestRequestBodyReplayErrorRouteNotAllowed verifies the default
// one-shot behavior: without error_routes in the replay policy, an
// error route cannot re-read the body the primary chain consumed.
func TestRequestBodyReplayErrorRouteNotAllowed(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			replay {
				memory 64
				max_size 1MB
			}
		}
		@hasbody vars_regexp {http.request.body} .
		handle @hasbody {
			error 503
		}
		handle_errors {
			respond "got:[{http.request.body}]" 200
		}
	}
  `, "caddyfile")

	tester.AssertPostResponseBody("http://localhost:9080/", nil, bytes.NewBufferString("consumed-then-errored"), http.StatusOK, "got:[]")
}

// TestRequestBodyReplayReject verifies the policy rejects an oversized
// upload with 413 when on_exceed is the default reject mode.
func TestRequestBodyReplayReject(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			replay {
				memory 8
				max_size 16
			}
		}
		respond "{{placeholder \"http.request.body\"}}"
		templates
	}
  `, "caddyfile")

	req, err := http.NewRequest(http.MethodPost, "http://localhost:9080/", bytes.NewBufferString(strings.Repeat("a", 100)))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	res := tester.AssertResponseCode(req, http.StatusRequestEntityTooLarge)
	res.Body.Close()
}

// TestRequestBodyReplayDegrade verifies degrade mode: bodies over the
// replay cap are still streamed normally to the downstream.
func TestRequestBodyReplayDegrade(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port     9080
		https_port    9443
		grace_period  1ns
	}
	http://localhost:9080 {
		request_body {
			replay {
				memory 8
				max_size 16
				on_exceed degrade
			}
		}
		respond "{http.request.body}"
	}
  `, "caddyfile")

	body := strings.Repeat("b", 100)
	tester.AssertPostResponseBody("http://localhost:9080/", nil, bytes.NewBufferString(body), http.StatusOK, body)
}
