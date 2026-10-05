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

package integration

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
)

func TestRewriteTransactionRollbackOnError(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
		admin localhost:2999
		http_port     9080
	}
	localhost:9080 {
		# this rewrite rolls back, so the header set afterwards is gone
		# by the time the error handler chain runs
		rewrite /rollback /routed-rb {
			transaction rollback_on_error
		}

		# this rewrite commits (the default), so the header survives into
		# the error handler chain, just like a plain rewrite
		rewrite /commit /routed-commit

		request_header X-Routed-Header yes
		error /routed-* "boom" 500

		handle_errors {
			respond "header={http.request.header.X-Routed-Header}|uri={http.request.uri}"
		}
	}`, "caddyfile")

	tester.AssertGetResponse("http://localhost:9080/rollback", 500, "header=|uri=/rollback")
	tester.AssertGetResponse("http://localhost:9080/commit", 500, "header=yes|uri=/commit")
}
