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

package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func testIncludeGraphPolicy() *IncludeGraphPolicy {
	compiledCache := true
	return &IncludeGraphPolicy{
		CompiledCache:     &compiledCache,
		CacheCapacity:     8,
		MaxFileSize:       1 << 20,
		MaxIncludeDepth:   8,
		MaxExpansionBytes: 1 << 20,
		CacheTTL:          caddy.Duration(time.Hour),
		EvictionPolicy:    evictionPolicyLRU,
	}
}

func managedTestContext(t *testing.T, policy *IncludeGraphPolicy) TemplateContext {
	t.Helper()
	if policy == nil {
		policy = testIncludeGraphPolicy()
	}
	cache, err := policy.provision("test")
	if err != nil {
		t.Fatalf("provisioning include graph policy: %v", err)
	}
	c := getContextOrFail(t)
	c.config = &Templates{
		IncludeGraph:  policy,
		funcVersion:   "test",
		templateCache: cache,
		logger:        zap.NewNop(),
	}
	t.Cleanup(func() { _ = c.config.Cleanup() })
	return c
}

func writeManagedTemplate(t *testing.T, c TemplateContext, name, content string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(string(c.Root.(http.Dir)), name), []byte(content), os.ModePerm)
	if err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func renderManagedString(t *testing.T, c *TemplateContext, name, content string) (string, error) {
	t.Helper()
	buf := bytes.NewBufferString(content)
	err := c.executeTemplateInBuffer(name, buf)
	return buf.String(), err
}

func TestIncludeGraphPolicyCaddyfile(t *testing.T) {
	raw := `templates {
		include_graph {
			compiled_cache false
			cache_capacity 128
			max_file_size 256KiB
			max_include_depth 16
			max_expansion_bytes 4MiB
			file_change_check 1s
			cache_ttl 5m
			eviction_policy random
		}
	}`
	helper := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(raw)}
	handler, err := parseCaddyfile(helper)
	if err != nil {
		t.Fatalf("parsing Caddyfile: %v", err)
	}
	tpl := handler.(*Templates)
	policy := tpl.IncludeGraph
	if policy == nil {
		t.Fatal("expected include graph policy")
	}
	if *policy.CompiledCache {
		t.Errorf("expected compiled cache to be disabled")
	}
	if policy.CacheCapacity != 128 {
		t.Errorf("expected capacity 128, got %d", policy.CacheCapacity)
	}
	if policy.MaxFileSize != 256*1024 {
		t.Errorf("expected max file size 256KiB, got %d", policy.MaxFileSize)
	}
	if policy.MaxIncludeDepth != 16 {
		t.Errorf("expected depth 16, got %d", policy.MaxIncludeDepth)
	}
	if policy.MaxExpansionBytes != 4*1024*1024 {
		t.Errorf("expected expansion 4MiB, got %d", policy.MaxExpansionBytes)
	}
	if time.Duration(policy.FileChangeCheck) != time.Second {
		t.Errorf("expected file check 1s, got %s", time.Duration(policy.FileChangeCheck))
	}
	if time.Duration(policy.CacheTTL) != 5*time.Minute {
		t.Errorf("expected TTL 5m, got %s", time.Duration(policy.CacheTTL))
	}
	if policy.EvictionPolicy != evictionPolicyRandom {
		t.Errorf("expected random eviction, got %q", policy.EvictionPolicy)
	}
}

func TestIncludeGraphPolicyCaddyfileSentinelsAndInvalidValues(t *testing.T) {
	helper := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(`
		templates {
			include_graph {
				file_change_check -1
			}
		}`)}
	handler, err := parseCaddyfile(helper)
	if err != nil {
		t.Fatalf("parsing file_check sentinel: %v", err)
	}
	if time.Duration(handler.(*Templates).IncludeGraph.FileChangeCheck) != -1 {
		t.Fatal("expected file_change_check to be -1")
	}

	for _, raw := range []string{
		`templates { include_graph { max_file_size 18446744073709551616 } }`,
		`templates { include_graph { compiled_cache maybe } }`,
		`templates { include_graph { unknown } }`,
	} {
		helper := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(raw)}
		if _, err := parseCaddyfile(helper); err == nil {
			t.Fatalf("expected invalid Caddyfile %q to fail", raw)
		}
	}
}

func TestIncludeGraphPolicyJSONAndDefaults(t *testing.T) {
	var tpl Templates
	raw := `{
		"include_graph": {
			"compiled_cache": true,
			"cache_capacity": 3,
			"max_file_size": 128,
			"max_include_depth": 4,
			"max_expansion_bytes": 256,
			"file_change_check": 1000000000,
			"cache_ttl": 60000000000,
			"eviction_policy": "random"
		}
	}`
	if err := json.Unmarshal([]byte(raw), &tpl); err != nil {
		t.Fatalf("unmarshalling JSON: %v", err)
	}
	policy := tpl.IncludeGraph
	if policy.CacheCapacity != 3 || policy.MaxFileSize != 128 || policy.MaxIncludeDepth != 4 || policy.MaxExpansionBytes != 256 {
		t.Fatalf("unexpected JSON policy: %+v", policy)
	}
	if time.Duration(policy.FileChangeCheck) != time.Second || time.Duration(policy.CacheTTL) != time.Minute {
		t.Fatalf("unexpected JSON durations: %+v", policy)
	}

	defaults := &IncludeGraphPolicy{}
	cache, err := defaults.provision("test")
	if err != nil {
		t.Fatalf("provisioning defaults: %v", err)
	}
	defer cache.close()
	if defaults.CompiledCache == nil || !*defaults.CompiledCache {
		t.Fatal("expected compiled cache to default to enabled")
	}
	if defaults.CacheCapacity != defaultCacheCapacity || defaults.MaxFileSize != defaultMaxFileSize || defaults.MaxIncludeDepth != defaultMaxIncludeDepth || defaults.MaxExpansionBytes != defaultMaxExpansion {
		t.Fatalf("unexpected default limits: %+v", defaults)
	}
	if time.Duration(defaults.FileChangeCheck) != 0 || time.Duration(defaults.CacheTTL) != defaultCacheTTL || defaults.EvictionPolicy != evictionPolicyLRU {
		t.Fatalf("unexpected default lifecycle: %+v", defaults)
	}

	invalid := &IncludeGraphPolicy{MaxIncludeDepth: -1}
	if _, err := invalid.provision("test"); err == nil {
		t.Fatal("expected invalid policy to fail provision")
	}
	if err := (&IncludeGraphPolicy{EvictionPolicy: "fifo"}).validate(); err == nil {
		t.Fatal("expected invalid eviction policy to fail validation")
	}
}

func TestIncludeGraphCompiledCacheHitAndIsolation(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "child", "cached")

	if _, err := renderManagedString(t, &c, "parent", `{{include "child"}}`); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if _, err := renderManagedString(t, &c, "parent", `{{include "child"}}`); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if hits := c.config.templateCache.hitCount(); hits != 1 {
		t.Fatalf("expected one cache hit, got %d", hits)
	}

	file, err := c.loadManagedTemplateFile(rootFileSystemKey(c.Root), "/child")
	if err != nil {
		t.Fatalf("loading cached file: %v", err)
	}
	key := c.cacheKeyFor(file)
	c.config.funcVersion = "other"
	if c.cacheKeyFor(file) == key {
		t.Fatal("function version was not part of the cache key")
	}
	c.config.Delimiters = []string{"[[", "]]"}
	if c.cacheKeyFor(file) == key {
		t.Fatal("delimiters were not part of the cache key")
	}
}

func TestIncludeGraphFileChangesInvalidateCache(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "child", "one")

	output, err := renderManagedString(t, &c, "parent", `{{include "child"}}`)
	if err != nil || output != "one" {
		t.Fatalf("first render = %q, %v", output, err)
	}
	output, err = renderManagedString(t, &c, "parent", `{{include "child"}}`)
	if err != nil || output != "one" {
		t.Fatalf("cached render = %q, %v", output, err)
	}

	writeManagedTemplate(t, c, "child", "two")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(string(c.Root.(http.Dir)), "child"), future, future); err != nil {
		t.Fatalf("changing mtime: %v", err)
	}
	output, err = renderManagedString(t, &c, "parent", `{{include "child"}}`)
	if err != nil || output != "two" {
		t.Fatalf("changed render = %q, %v", output, err)
	}
	if c.config.templateCache.len() != 1 {
		t.Fatalf("expected one current cache entry, got %d", c.config.templateCache.len())
	}

	replacement := filepath.Join(string(c.Root.(http.Dir)), "replacement")
	writeManagedTemplate(t, c, "replacement", "three")
	if err := os.Rename(replacement, filepath.Join(string(c.Root.(http.Dir)), "child")); err != nil {
		t.Fatalf("replacing child: %v", err)
	}
	output, err = renderManagedString(t, &c, "parent", `{{include "child"}}`)
	if err != nil || output != "three" {
		t.Fatalf("replaced render = %q, %v", output, err)
	}
	if c.config.templateCache.len() != 1 {
		t.Fatalf("expected replacement to remove old entry, got %d", c.config.templateCache.len())
	}
}

func TestIncludeGraphCacheCapacityTTLAndCheckInterval(t *testing.T) {
	policy := testIncludeGraphPolicy()
	policy.CacheCapacity = 2
	c := managedTestContext(t, policy)
	for _, name := range []string{"one", "two"} {
		writeManagedTemplate(t, c, name, name)
	}
	for _, source := range []string{`{{include "one"}}`, `{{include "two"}}`, `{{include "one"}}`} {
		if _, err := renderManagedString(t, &c, "parent", source); err != nil {
			t.Fatal(err)
		}
	}
	writeManagedTemplate(t, c, "three", "three")
	if _, err := renderManagedString(t, &c, "parent", `{{include "three"}}`); err != nil {
		t.Fatal(err)
	}
	one, err := c.loadManagedTemplateFile(rootFileSystemKey(c.Root), "/one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := c.loadManagedTemplateFile(rootFileSystemKey(c.Root), "/two")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.config.templateCache.entries[c.cacheKeyFor(one)]; !ok {
		t.Fatal("LRU unexpectedly evicted recently used one")
	}
	if _, ok := c.config.templateCache.entries[c.cacheKeyFor(two)]; ok {
		t.Fatal("LRU did not evict two")
	}

	shortTTL := testIncludeGraphPolicy()
	shortTTL.CacheTTL = caddy.Duration(10 * time.Millisecond)
	expiring := managedTestContext(t, shortTTL)
	writeManagedTemplate(t, expiring, "child", "x")
	if _, err := renderManagedString(t, &expiring, "parent", `{{include "child"}}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := renderManagedString(t, &expiring, "parent", `{{include "child"}}`); err != nil {
		t.Fatal(err)
	}
	if hits := expiring.config.templateCache.hitCount(); hits != 0 {
		t.Fatalf("expected TTL miss, got hits %d", hits)
	}

	skipCheck := testIncludeGraphPolicy()
	skipCheck.FileChangeCheck = caddy.Duration(-1)
	stale := managedTestContext(t, skipCheck)
	writeManagedTemplate(t, stale, "child", "first")
	if _, err := renderManagedString(t, &stale, "parent", `{{include "child"}}`); err != nil {
		t.Fatal(err)
	}
	writeManagedTemplate(t, stale, "child", "second")
	output, err := renderManagedString(t, &stale, "parent", `{{include "child"}}`)
	if err != nil || output != "first" {
		t.Fatalf("expected stale entry within check interval, got %q, %v", output, err)
	}
}

func TestIncludeGraphReuseAndCycles(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "shared", "x")
	writeManagedTemplate(t, c, "left", `{{include "shared"}}`)
	writeManagedTemplate(t, c, "right", `{{include "shared"}}`)
	output, err := renderManagedString(t, &c, "parent", `{{include "left"}}{{include "right"}}`)
	if err != nil || output != "xx" {
		t.Fatalf("DAG render = %q, %v", output, err)
	}

	writeManagedTemplate(t, c, "cycle-a", `{{include "cycle-b"}}`)
	writeManagedTemplate(t, c, "cycle-b", `{{include "cycle-a"}}`)
	_, err = renderManagedString(t, &c, "parent", `{{include "cycle-a"}}`)
	if !errors.Is(err, errIncludeCycle) {
		t.Fatalf("expected include cycle error, got %v", err)
	}

	writeManagedTemplate(t, c, "import-a", `{{define "a"}}{{import "import-b"}}{{template "b"}}{{end}}`)
	writeManagedTemplate(t, c, "import-b", `{{define "b"}}{{import "import-a"}}{{end}}`)
	_, err = renderManagedString(t, &c, "parent", `{{import "import-a"}}{{template "a"}}`)
	if !errors.Is(err, errIncludeCycle) {
		t.Fatalf("expected import cycle error, got %v", err)
	}
}

func TestIncludeGraphLimitsAndAtomicOutput(t *testing.T) {
	policy := testIncludeGraphPolicy()
	policy.MaxFileSize = 4
	policy.MaxIncludeDepth = 1
	c := managedTestContext(t, policy)
	writeManagedTemplate(t, c, "large", "12345")
	_, err := renderManagedString(t, &c, "parent", `{{include "large"}}`)
	if !errors.Is(err, errMaxFileSize) {
		t.Fatalf("expected file size error, got %v", err)
	}
	depthPolicy := testIncludeGraphPolicy()
	depthPolicy.MaxIncludeDepth = 1
	depthContext := managedTestContext(t, depthPolicy)
	writeManagedTemplate(t, depthContext, "child", `{{include "grandchild"}}`)
	writeManagedTemplate(t, depthContext, "grandchild", "x")
	_, err = renderManagedString(t, &depthContext, "parent", `{{include "child"}}`)
	if !errors.Is(err, errMaxIncludeDepth) {
		t.Fatalf("expected depth error, got %v", err)
	}

	limited := managedTestContext(t, &IncludeGraphPolicy{
		CacheCapacity:     4,
		MaxFileSize:       1 << 20,
		MaxIncludeDepth:   8,
		MaxExpansionBytes: 5,
		CacheTTL:          caddy.Duration(time.Hour),
		EvictionPolicy:    evictionPolicyLRU,
	})
	source := "123456"
	output, err := renderManagedString(t, &limited, "parent", source)
	if !errors.Is(err, errMaxExpansionSize) {
		t.Fatalf("expected expansion error, got %v", err)
	}
	if output != source {
		t.Fatalf("failed render must preserve destination buffer, got %q", output)
	}
}

func TestIncludeGraphExpansionAccounting(t *testing.T) {
	policy := testIncludeGraphPolicy()
	policy.MaxExpansionBytes = 15
	c := managedTestContext(t, policy)
	writeManagedTemplate(t, c, "expanded-child", "0123456789")
	writeManagedTemplate(t, c, "expanded-parent", `{{include "expanded-child"}}`)

	_, err := renderManagedString(t, &c, "parent", `{{include "expanded-parent"}}XXXXXXXXXX`)
	if !errors.Is(err, errMaxExpansionSize) {
		t.Fatalf("expected nested expansion limit, got %v", err)
	}

	within := testIncludeGraphPolicy()
	within.MaxExpansionBytes = 20
	withinContext := managedTestContext(t, within)
	writeManagedTemplate(t, withinContext, "expanded-child", "0123456789")
	writeManagedTemplate(t, withinContext, "expanded-parent", `{{include "expanded-child"}}`)
	output, err := renderManagedString(t, &withinContext, "parent", `{{include "expanded-parent"}}XXXXXXXXXX`)
	if err != nil || output != "0123456789XXXXXXXXXX" {
		t.Fatalf("boundary render = %q, %v", output, err)
	}

	siblings := testIncludeGraphPolicy()
	siblings.MaxExpansionBytes = 20
	siblingsContext := managedTestContext(t, siblings)
	writeManagedTemplate(t, siblingsContext, "sibling-one", "0123456789")
	writeManagedTemplate(t, siblingsContext, "sibling-two", "0123456789")
	output, err = renderManagedString(t, &siblingsContext, "parent", `{{include "sibling-one"}}{{include "sibling-two"}}`)
	if err != nil || output != "01234567890123456789" {
		t.Fatalf("sibling render = %q, %v", output, err)
	}
}

func TestIncludeGraphImportReuseDepthAndDuplicateDefinitions(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "shared-import", `{{define "shared"}}shared{{end}}`)
	writeManagedTemplate(t, c, "import-left", `{{define "left"}}{{import "shared-import"}}left{{template "shared"}}{{end}}`)
	writeManagedTemplate(t, c, "import-right", `{{define "right"}}{{import "shared-import"}}right{{template "shared"}}{{end}}`)
	output, err := renderManagedString(t, &c, "parent", `{{import "import-left"}}{{import "import-right"}}{{template "left"}}{{template "right"}}`)
	if err != nil || output != "leftsharedrightshared" {
		t.Fatalf("import DAG render = %q, %v", output, err)
	}

	repeated := managedTestContext(t, nil)
	writeManagedTemplate(t, repeated, "repeat-import", `{{define "repeat"}}repeat{{end}}`)
	output, err = renderManagedString(t, &repeated, "parent", `{{import "repeat-import"}}{{template "repeat"}}{{import "repeat-import"}}{{template "repeat"}}`)
	if err != nil || output != "repeatrepeat" {
		t.Fatalf("repeated import render = %q, %v", output, err)
	}

	depthPolicy := testIncludeGraphPolicy()
	depthPolicy.MaxIncludeDepth = 2
	depthContext := managedTestContext(t, depthPolicy)
	writeManagedTemplate(t, depthContext, "chain1", `{{define "chain1"}}{{import "chain2"}}{{template "chain2"}}{{end}}`)
	writeManagedTemplate(t, depthContext, "chain2", `{{define "chain2"}}{{import "chain3"}}two{{template "chain3"}}{{end}}`)
	writeManagedTemplate(t, depthContext, "chain3", `{{define "chain3"}}three{{end}}`)
	_, err = renderManagedString(t, &depthContext, "parent", `{{import "chain1"}}{{template "chain1"}}`)
	if !errors.Is(err, errMaxIncludeDepth) {
		t.Fatalf("expected import depth limit, got %v", err)
	}

	allowedPolicy := testIncludeGraphPolicy()
	allowedPolicy.MaxIncludeDepth = 3
	allowedContext := managedTestContext(t, allowedPolicy)
	writeManagedTemplate(t, allowedContext, "chain1", `{{define "chain1"}}{{import "chain2"}}{{template "chain2"}}{{end}}`)
	writeManagedTemplate(t, allowedContext, "chain2", `{{define "chain2"}}{{import "chain3"}}two{{template "chain3"}}{{end}}`)
	writeManagedTemplate(t, allowedContext, "chain3", `{{define "chain3"}}three{{end}}`)
	output, err = renderManagedString(t, &allowedContext, "parent", `{{import "chain1"}}{{template "chain1"}}`)
	if err != nil || output != "twothree" {
		t.Fatalf("import chain within depth = %q, %v", output, err)
	}
}

func TestIncludeGraphExecutionErrorsDoNotPopulateCache(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "failing", `{{httpError 500}}`)
	_, err := renderManagedString(t, &c, "parent", `{{include "failing"}}`)
	if err == nil {
		t.Fatal("expected execution error")
	}
	if c.config.templateCache.len() != 0 {
		t.Fatalf("execution error populated shared cache: %d entries", c.config.templateCache.len())
	}
}

func TestIncludeGraphHTTPExpansionBoundary(t *testing.T) {
	policy := testIncludeGraphPolicy()
	policy.MaxExpansionBytes = 10
	c := managedTestContext(t, policy)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	})
	requestCtx := context.WithValue(c.Req.Context(), caddyhttp.ServerCtxKey, handler)
	c.Req = c.Req.WithContext(requestCtx)
	output, err := c.funcHTTPInclude("/boundary")
	if err != nil || output != "0123456789" {
		t.Fatalf("boundary httpInclude = %q, %v", output, err)
	}
}

func TestIncludeGraphRenderedHTTPIncludeBoundary(t *testing.T) {
	policy := testIncludeGraphPolicy()
	policy.MaxExpansionBytes = 10
	c := managedTestContext(t, policy)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(templatesRenderedHeader, "1")
		_, _ = w.Write([]byte("0123456789"))
	})
	requestCtx := context.WithValue(c.Req.Context(), caddyhttp.ServerCtxKey, handler)
	c.Req = c.Req.WithContext(requestCtx)
	output, err := c.funcHTTPInclude("/rendered")
	if err != nil || output != "0123456789" {
		t.Fatalf("rendered httpInclude = %q, %v", output, err)
	}

	oversized := managedTestContext(t, policy)
	oversizedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("01234567890"))
	})
	oversizedCtx := context.WithValue(oversized.Req.Context(), caddyhttp.ServerCtxKey, oversizedHandler)
	oversized.Req = oversized.Req.WithContext(oversizedCtx)
	_, err = oversized.funcHTTPInclude("/oversized")
	if !errors.Is(err, errMaxExpansionSize) {
		t.Fatalf("expected oversized virtual response error, got %v", err)
	}
}

func TestIncludeGraphImportDepthMemoAndIncludeEdges(t *testing.T) {
	policy := testIncludeGraphPolicy()
	policy.MaxIncludeDepth = 4
	c := managedTestContext(t, policy)
	writeManagedTemplate(t, c, "memo-f", `{{define "f"}}F{{end}}`)
	writeManagedTemplate(t, c, "memo-e", `{{define "e"}}{{import "memo-f"}}{{template "f"}}E{{end}}`)
	writeManagedTemplate(t, c, "memo-d", `{{define "d"}}{{import "memo-e"}}{{template "e"}}D{{end}}`)
	writeManagedTemplate(t, c, "memo-c", `{{define "c"}}{{import "memo-d"}}{{template "d"}}{{end}}`)
	writeManagedTemplate(t, c, "memo-b", `{{define "b"}}{{import "memo-c"}}{{template "c"}}{{end}}{{template "b"}}`)
	_, err := renderManagedString(t, &c, "parent", `{{import "memo-d"}}{{template "d"}}{{include "memo-b"}}`)
	if !errors.Is(err, errMaxIncludeDepth) {
		t.Fatalf("expected memoized deep import path to fail, got %v", err)
	}

	edgePolicy := testIncludeGraphPolicy()
	edgePolicy.MaxIncludeDepth = 2
	edgeContext := managedTestContext(t, edgePolicy)
	writeManagedTemplate(t, edgeContext, "edge-p", `{{import "edge-a"}}{{template "edge-a"}}`)
	writeManagedTemplate(t, edgeContext, "edge-a", `{{define "edge-a"}}{{import "edge-b"}}B{{template "edge-b"}}{{end}}`)
	writeManagedTemplate(t, edgeContext, "edge-b", `{{define "edge-b"}}B{{end}}`)
	_, err = renderManagedString(t, &edgeContext, "parent", `{{include "edge-p"}}`)
	if !errors.Is(err, errMaxIncludeDepth) {
		t.Fatalf("expected include+import shared depth limit, got %v", err)
	}

	allowedPolicy := testIncludeGraphPolicy()
	allowedPolicy.MaxIncludeDepth = 3
	allowedContext := managedTestContext(t, allowedPolicy)
	writeManagedTemplate(t, allowedContext, "edge-p", `{{import "edge-a"}}{{template "edge-a"}}`)
	writeManagedTemplate(t, allowedContext, "edge-a", `{{define "edge-a"}}{{import "edge-b"}}B{{template "edge-b"}}{{end}}`)
	writeManagedTemplate(t, allowedContext, "edge-b", `{{define "edge-b"}}B{{end}}`)
	output, err := renderManagedString(t, &allowedContext, "parent", `{{include "edge-p"}}`)
	if err != nil || output != "BB" {
		t.Fatalf("include+import within depth = %q, %v", output, err)
	}
}

func TestIncludeGraphCacheRootIsolationAndConcurrency(t *testing.T) {
	policy := testIncludeGraphPolicy()
	cache, err := policy.provision("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.close)
	c := managedTestContext(t, testIncludeGraphPolicy())
	c.config.templateCache = cache
	writeManagedTemplate(t, c, "shared", "one")
	file, err := c.loadManagedTemplateFile(rootFileSystemKey(c.Root), "/shared")
	if err != nil {
		t.Fatal(err)
	}
	firstKey := c.cacheKeyFor(file)

	other := managedTestContext(t, testIncludeGraphPolicy())
	writeManagedTemplate(t, other, "shared", "two")
	other.config.templateCache = cache
	otherFile, err := other.loadManagedTemplateFile(rootFileSystemKey(other.Root), "/shared")
	if err != nil {
		t.Fatal(err)
	}
	if other.cacheKeyFor(otherFile) == firstKey {
		t.Fatal("cache key did not isolate different roots")
	}

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "shared"), []byte("concurrent"), os.ModePerm); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
			if err != nil {
				t.Error(err)
				return
			}
			renderContext := TemplateContext{
				Root:       http.Dir(base),
				Req:        request,
				RespHeader: WrappedHeader{Header: make(http.Header)},
				config: &Templates{
					IncludeGraph:  policy,
					funcVersion:   "test",
					templateCache: cache,
					logger:        zap.NewNop(),
				},
			}
			output, err := renderManagedString(t, &renderContext, "parent", `{{include "shared"}}`)
			if err != nil || output != "concurrent" {
				t.Errorf("concurrent render = %q, %v", output, err)
			}
		}()
	}
	wg.Wait()
}

func TestIncludeGraphParseErrorsDoNotPolluteCache(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "broken", `{{if true}}`)
	_, err := renderManagedString(t, &c, "parent", `{{include "broken"}}`)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if c.config.templateCache.len() != 0 {
		t.Fatalf("parse error populated cache: %d entries", c.config.templateCache.len())
	}
	writeManagedTemplate(t, c, "broken", "ok")
	output, err := renderManagedString(t, &c, "parent", `{{include "broken"}}`)
	if err != nil || output != "ok" {
		t.Fatalf("recovery render = %q, %v", output, err)
	}
}

func TestIncludeGraphHTTPIncludeAndCancellation(t *testing.T) {
	c := managedTestContext(t, nil)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("good contents"))
	})
	requestCtx := context.WithValue(c.Req.Context(), caddyhttp.ServerCtxKey, handler)
	c.Req = c.Req.WithContext(requestCtx)
	output, err := c.funcHTTPInclude("https://example.com/x")
	if err != nil || output != "good contents" {
		t.Fatalf("managed httpInclude = %q, %v", output, err)
	}

	cyclic := managedTestContext(t, nil)
	cyclicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{{httpInclude "/x"}}`))
	})
	cyclicCtx := context.WithValue(cyclic.Req.Context(), caddyhttp.ServerCtxKey, cyclicHandler)
	cyclic.Req = cyclic.Req.WithContext(cyclicCtx)
	_, err = cyclic.funcHTTPInclude("/x")
	if !errors.Is(err, errIncludeCycle) {
		t.Fatalf("expected HTTP cycle error, got %v", err)
	}

	canceled := managedTestContext(t, nil)
	cancelableCtx, cancel := context.WithCancel(context.WithValue(canceled.Req.Context(), caddyhttp.ServerCtxKey, handler))
	cancel()
	canceled.Req = canceled.Req.WithContext(cancelableCtx)
	_, err = canceled.funcHTTPInclude("/x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
	if canceled.config.templateCache.len() != 0 {
		t.Fatalf("canceled render populated cache: %d entries", canceled.config.templateCache.len())
	}
}

func TestIncludeGraphCleanupIsolatesInstances(t *testing.T) {
	c := managedTestContext(t, nil)
	writeManagedTemplate(t, c, "child", "x")
	if _, err := renderManagedString(t, &c, "parent", `{{include "child"}}`); err != nil {
		t.Fatal(err)
	}
	oldCache := c.config.templateCache
	oldCache.close()
	if oldCache.len() != 0 {
		t.Fatal("cleanup did not clear cache")
	}
	if _, err := renderManagedString(t, &c, "parent", `{{include "child"}}`); err != nil {
		t.Fatal(err)
	}
	if oldCache.len() != 0 {
		t.Fatal("closed cache accepted a new entry")
	}

	other := managedTestContext(t, nil)
	if other.config.templateCache == oldCache {
		t.Fatal("new provisioned instance reused old cache")
	}
}
