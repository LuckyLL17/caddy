package integration

import (
	"net/http"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
)

func TestTryHandleFallbackStatus(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		admin localhost:2999
		http_port 9080
	}
	localhost:9080 {
		try_handle {
			fallback_status 502 503
			handle /cache/* {
				header X-Branch cache
				respond "bad" 502
			}
			handle {
				header X-Branch main
				respond "good" 200
			}
		}
	}
	`, "caddyfile")

	// the first branch matches and fails with 502, so the second one runs
	tester.AssertGetResponse("http://localhost:9080/cache/x", http.StatusOK, "good")
}

func TestTryHandleFallbackExhaustedError(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		admin localhost:2999
		http_port 9080
	}
	localhost:9080 {
		try_handle {
			fallback_status 502
			on_exhausted error
			handle {
				respond "bad" 502
			}
		}
	}
	`, "caddyfile")

	// every candidate failed and the plan asked to surface an error;
	// the server error chain renders the last status code
	tester.AssertGetResponse("http://localhost:9080/", http.StatusBadGateway, "")
}

func TestTryHandleNoMatchRunsNext(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		admin localhost:2999
		http_port 9080
	}
	localhost:9080 {
		try_handle {
			fallback_status 502
			handle /cache/* {
				respond "bad" 502
			}
		}
		respond "outside" 200
	}
	`, "caddyfile")

	// no branch matches, so subsequent outer handlers still run
	tester.AssertGetResponse("http://localhost:9080/other", http.StatusOK, "outside")
}
