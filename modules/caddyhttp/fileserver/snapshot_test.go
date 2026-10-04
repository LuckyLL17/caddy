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

package fileserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// testSnapshotManifest mirrors the on-wire manifest shape for tests.
type testSnapshotManifest struct {
	Version      int             `json:"version"`
	Path         string          `json:"path"`
	Hash         string          `json:"hash"`
	LastModified time.Time       `json:"last_modified"`
	EntryCount   int             `json:"entry_count"`
	Digest       string          `json:"digest"`
	Entries      []snapshotEntry `json:"entries"`
}

// snapshotTree builds a representative tree and returns its root.
// Layout:
//
//	a.txt            ("hello\n")
//	sub/b.txt        ("world\n")
//	hiddir/x.txt     (hidden by component name)
//	.secret          (hidden by component name)
func snapshotTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "hiddir"), 0o755))
	must(os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("world\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "deep", "c.txt"), []byte("c"), 0o644))
	must(os.WriteFile(filepath.Join(root, "hiddir", "x.txt"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".secret"), []byte("shh"), 0o644))
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{"a.txt", filepath.Join("sub", "b.txt"), filepath.Join("sub", "deep", "c.txt")} {
		must(os.Chtimes(filepath.Join(root, p), past, past))
	}
	return root
}

func newSnapshotFileServer(t *testing.T, root string, snap *Snapshot) *FileServer {
	t.Helper()
	fsrv := &FileServer{
		Root:     root,
		Hide:     []string{"hiddir", ".secret"},
		Snapshot: snap,
	}
	ctx, _ := caddy.NewContext(caddy.Context{Context: context.Background()})
	if err := fsrv.Provision(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	return fsrv
}

func newSnapshotRequest(t *testing.T, method, target string, headers ...[2]string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		r.Header.Set(h[0], h[1])
	}
	ctx := context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer())
	ctx = context.WithValue(ctx, caddyhttp.OriginalRequestCtxKey, *r)
	return r.WithContext(ctx)
}

func doSnapshotRequest(t *testing.T, fsrv *FileServer, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	if err := fsrv.ServeHTTP(w, r, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return w
}

func decodeManifest(t *testing.T, body []byte) testSnapshotManifest {
	t.Helper()
	var m testSnapshotManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decoding manifest: %v\nbody: %s", err, body)
	}
	return m
}

func entryIndex(m testSnapshotManifest, p string) int {
	for i, e := range m.Entries {
		if e.Path == p {
			return i
		}
	}
	return -1
}

func TestSnapshotManifest(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})

	w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != snapshotMediaType+"; charset=utf-8" {
		t.Errorf("unexpected content type %q", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("expected nosniff, got %q", w.Header().Get("X-Content-Type-Options"))
	}
	etag := w.Header().Get("ETag")
	if etag == "" || etag[0] != '"' {
		t.Fatalf("expected quoted ETag, got %q", etag)
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Error("expected Last-Modified header")
	}

	m := decodeManifest(t, w.Body.Bytes())
	if m.Version != snapshotManifestVersion {
		t.Errorf("expected version %d, got %d", snapshotManifestVersion, m.Version)
	}
	if m.Hash != snapshotHashMetadata {
		t.Errorf("expected metadata hash, got %q", m.Hash)
	}
	if m.Path != "/" {
		t.Errorf("expected path /, got %q", m.Path)
	}
	if etag != `"`+m.Digest+`"` {
		t.Errorf("ETag %q does not match manifest digest %q", etag, m.Digest)
	}
	if m.EntryCount != len(m.Entries) {
		t.Errorf("entry_count %d does not match %d entries", m.EntryCount, len(m.Entries))
	}

	want := map[string]string{
		".":              "dir",
		"a.txt":          "file",
		"sub":            "dir",
		"sub/b.txt":      "file",
		"sub/deep":       "dir",
		"sub/deep/c.txt": "file",
	}
	if len(m.Entries) != len(want) {
		var paths []string
		for _, e := range m.Entries {
			paths = append(paths, e.Path)
		}
		t.Fatalf("expected %d entries, got %d: %v", len(want), len(m.Entries), paths)
	}
	for p, typ := range want {
		i := entryIndex(m, p)
		if i < 0 {
			t.Errorf("missing entry %q", p)
			continue
		}
		if m.Entries[i].Type != typ {
			t.Errorf("entry %q: expected type %q, got %q", p, typ, m.Entries[i].Type)
		}
	}
	for _, hidden := range []string{".secret", "hiddir", "hiddir/x.txt"} {
		if entryIndex(m, hidden) >= 0 {
			t.Errorf("hidden entry %q should not be in manifest", hidden)
		}
	}

	// entries must be in deterministic walk order (lexical DFS)
	if m.Entries[0].Path != "." {
		t.Errorf("root record must come first, got %q", m.Entries[0].Path)
	}
	for i := 1; i < len(m.Entries); i++ {
		if m.Entries[i-1].Path > m.Entries[i].Path {
			t.Errorf("entries not sorted: %q after %q", m.Entries[i].Path, m.Entries[i-1].Path)
		}
	}

	// the per-file ETag must be the very same validator the file
	// server returns when serving that file
	info, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	a := m.Entries[entryIndex(m, "a.txt")]
	if a.ETag != calculateEtag(info) {
		t.Errorf("entry ETag %q != file server ETag %q", a.ETag, calculateEtag(info))
	}
	if a.Size != info.Size() {
		t.Errorf("entry size %d != %d", a.Size, info.Size())
	}
	if a.Digest != "" {
		t.Errorf("metadata mode must not include content digests, got %q", a.Digest)
	}
}

func TestSnapshotConditionalRequests(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})

	first := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	etag := first.Header().Get("ETag")
	body := first.Body.Bytes()

	for _, tc := range []struct {
		name   string
		header [2]string
		want   int
	}{
		{"if-none-match hit", [2]string{"If-None-Match", etag}, http.StatusNotModified},
		{"if-none-match miss", [2]string{"If-None-Match", `"does-not-match"`}, http.StatusOK},
		{"if-match hit", [2]string{"If-Match", etag}, http.StatusOK},
		{"if-modified-since future", [2]string{"If-Modified-Since", time.Now().UTC().Format(http.TimeFormat)}, http.StatusNotModified},
		{"if-modified-since past", [2]string{"If-Modified-Since", time.Unix(0, 0).UTC().Format(http.TimeFormat)}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot", tc.header))
			if w.Code != tc.want {
				t.Errorf("expected %d, got %d: %s", tc.want, w.Code, w.Body.String())
			}
		})
	}

	// a change in the tree must change the manifest ETag
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello, changed!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "a.txt"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	second := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	if second.Header().Get("ETag") == etag {
		t.Error("ETag did not change after file modification")
	}
	if bytes.Equal(second.Body.Bytes(), body) {
		t.Error("manifest body did not change after file modification")
	}

	// stale validator no longer satisfies If-None-Match
	stale := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot", [2]string{"If-None-Match", etag}))
	if stale.Code != http.StatusOK {
		t.Errorf("expected 200 with stale validator, got %d", stale.Code)
	}
}

func TestSnapshotSHA256(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{Hash: snapshotHashSHA256})

	w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	m := decodeManifest(t, w.Body.Bytes())
	if m.Hash != snapshotHashSHA256 {
		t.Fatalf("expected sha256 hash mode, got %q", m.Hash)
	}

	sum := sha256.Sum256([]byte("hello\n"))
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	a := m.Entries[entryIndex(m, "a.txt")]
	if a.Digest != wantDigest {
		t.Errorf("expected digest %q, got %q", wantDigest, a.Digest)
	}
	if a.ETag == "" {
		t.Error("expected per-file ETag even in sha256 mode")
	}
	dir := m.Entries[entryIndex(m, "sub")]
	if dir.Digest != "" {
		t.Errorf("directories must not carry content digests, got %q", dir.Digest)
	}
}

func TestSnapshotHEAD(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})

	get := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	head := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodHead, "/?snapshot"))

	if head.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD must not return a body, got %d bytes", head.Body.Len())
	}
	if head.Header().Get("Content-Length") != fmt.Sprintf("%d", get.Body.Len()) {
		t.Errorf("HEAD Content-Length %q != GET body size %d",
			head.Header().Get("Content-Length"), get.Body.Len())
	}
	if head.Header().Get("ETag") != get.Header().Get("ETag") {
		t.Errorf("HEAD ETag %q != GET ETag %q", head.Header().Get("ETag"), get.Header().Get("ETag"))
	}
}

func TestSnapshotMethodNotAllowed(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})
	r := newSnapshotRequest(t, http.MethodPost, "/?snapshot")
	w := httptest.NewRecorder()
	err := fsrv.ServeHTTP(w, r, nil)
	herr, ok := err.(caddyhttp.HandlerError)
	if !ok || herr.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 handler error, got %T %v", err, err)
	}
	if w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("expected Allow header, got %q", w.Header().Get("Allow"))
	}
}

func TestSnapshotContentNegotiation(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})

	// Accept header alone triggers the manifest
	w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/",
		[2]string{"Accept", "application/json, " + snapshotMediaType}))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 via content negotiation, got %d", w.Code)
	}
	decodeManifest(t, w.Body.Bytes())

	// an ordinary request without the marker follows normal file server
	// behavior: no index file, no browse => 404 handler error
	r := newSnapshotRequest(t, http.MethodGet, "/")
	rec := httptest.NewRecorder()
	err := fsrv.ServeHTTP(rec, r, nil)
	if herr, ok := err.(caddyhttp.HandlerError); !ok || herr.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for plain directory request, got %T %v", err, err)
	}
}

func TestSnapshotMarkerIgnoredForFiles(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})

	w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/a.txt?snapshot"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "hello\n" {
		t.Errorf("expected file contents, got %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("expected plain text file response, got %q", ct)
	}
}

func TestSnapshotSymlinks(t *testing.T) {
	root := snapshotTree(t)

	linkFile := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(root, "a.txt"), linkFile); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	linkDir := filepath.Join(root, "linksub")
	if err := os.Symlink(filepath.Join(root, "sub"), linkDir); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// default: symlinks reported but not followed, targets hidden
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})
	w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	m := decodeManifest(t, w.Body.Bytes())

	link := m.Entries[entryIndex(m, "link.txt")]
	if link.Type != "symlink" || link.TargetType != "file" {
		t.Errorf("expected symlink to file, got type=%q target=%q", link.Type, link.TargetType)
	}
	if link.SymlinkPath != "" {
		t.Errorf("symlink target must not be revealed by default, got %q", link.SymlinkPath)
	}
	targetInfo, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if link.ETag != calculateEtag(targetInfo) {
		t.Errorf("symlink ETag %q should match target validator %q", link.ETag, calculateEtag(targetInfo))
	}

	dirLink := m.Entries[entryIndex(m, "linksub")]
	if dirLink.Type != "symlink" || dirLink.TargetType != "dir" {
		t.Errorf("expected symlink to dir, got type=%q target=%q", dirLink.Type, dirLink.TargetType)
	}
	if entryIndex(m, "linksub/b.txt") >= 0 {
		t.Error("symlinked directory must not be traversed by default")
	}

	dangling := m.Entries[entryIndex(m, "dangling")]
	if dangling.Type != "symlink" || dangling.TargetType != "broken" {
		t.Errorf("expected broken symlink, got type=%q target=%q", dangling.Type, dangling.TargetType)
	}
	if dangling.ETag != "" {
		t.Errorf("broken symlink must not carry ETag, got %q", dangling.ETag)
	}

	// reveal_symlinks exposes the raw target
	fsrv = newSnapshotFileServer(t, root, &Snapshot{RevealSymlinks: true})
	w = doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	m = decodeManifest(t, w.Body.Bytes())
	link = m.Entries[entryIndex(m, "link.txt")]
	if link.SymlinkPath != filepath.Join(root, "a.txt") {
		t.Errorf("expected revealed target, got %q", link.SymlinkPath)
	}

	// follow_symlinks traverses symlinked directories
	fsrv = newSnapshotFileServer(t, root, &Snapshot{FollowSymlinks: true, RevealSymlinks: true})
	w = doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	m = decodeManifest(t, w.Body.Bytes())
	if i := entryIndex(m, "linksub/b.txt"); i < 0 {
		t.Error("symlinked directory contents missing with follow_symlinks")
	} else if m.Entries[i].Digest != "" {
		// metadata mode never has content digests
		t.Error("unexpected content digest in metadata mode")
	}
}

func TestSnapshotSymlinkCycle(t *testing.T) {
	root := snapshotTree(t)

	// linkloop -> parent directory, which contains linkloop itself
	if err := os.Symlink(root, filepath.Join(root, "sub", "loop")); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	fsrv := newSnapshotFileServer(t, root, &Snapshot{FollowSymlinks: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/?snapshot"))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot walk did not terminate on a symlink cycle")
	}
}

func TestSnapshotSubdirectory(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{})

	// request without trailing slash must redirect like browse
	redir := httptest.NewRecorder()
	r := newSnapshotRequest(t, http.MethodGet, "/sub?snapshot")
	if err := fsrv.ServeHTTP(redir, r, nil); err != nil {
		t.Fatal(err)
	}
	if redir.Code != http.StatusPermanentRedirect {
		t.Fatalf("expected redirect, got %d", redir.Code)
	}
	if loc := redir.Header().Get("Location"); loc != "/sub/?snapshot" {
		t.Errorf("unexpected redirect location %q", loc)
	}

	w := doSnapshotRequest(t, fsrv, newSnapshotRequest(t, http.MethodGet, "/sub/?snapshot"))
	m := decodeManifest(t, w.Body.Bytes())
	if m.Path != "/sub/" {
		t.Errorf("expected path /sub/, got %q", m.Path)
	}
	for _, p := range []string{".", "b.txt", "deep", "deep/c.txt"} {
		if entryIndex(m, p) < 0 {
			t.Errorf("missing %q in subdirectory snapshot", p)
		}
	}
	if entryIndex(m, "a.txt") >= 0 {
		t.Error("snapshot must be rooted at the requested directory")
	}
}

func TestSnapshotFileLimit(t *testing.T) {
	root := snapshotTree(t)
	fsrv := newSnapshotFileServer(t, root, &Snapshot{FileLimit: 1})
	r := newSnapshotRequest(t, http.MethodGet, "/?snapshot")
	w := httptest.NewRecorder()
	err := fsrv.ServeHTTP(w, r, nil)
	herr, ok := err.(caddyhttp.HandlerError)
	if !ok || herr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when file_limit exceeded, got %T %v", err, err)
	}
}

func TestSnapshotCaddyfile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		hash      string
		follow    bool
		reveal    bool
		fileLimit int
		wantErr   bool
	}{
		{
			name:  "inline",
			input: `file_server snapshot`,
			hash:  snapshotHashMetadata,
		},
		{
			name:      "sha256 block",
			input:     "file_server {\n\tsnapshot sha256 {\n\t\tfollow_symlinks\n\t\treveal_symlinks\n\t\tfile_limit 42\n\t}\n}",
			hash:      snapshotHashSHA256,
			follow:    true,
			reveal:    true,
			fileLimit: 42,
		},
		{
			name:    "duplicate",
			input:   "file_server {\n\tsnapshot\n\tsnapshot\n}",
			wantErr: true,
		},
		{
			name:    "bad hash",
			input:   "file_server {\n\tsnapshot bogus\n}",
			wantErr: false, // parse succeeds; provisioning rejects it
			hash:    "bogus",
		},
		{
			name:    "unknown subdirective",
			input:   "file_server {\n\tsnapshot {\n\t\tbogus\n\t}\n}",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsrv := new(FileServer)
			err := fsrv.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected parse error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if fsrv.Snapshot == nil {
				t.Fatal("snapshot not configured")
			}
			if fsrv.Snapshot.Hash != tc.hash {
				t.Errorf("hash: got %q want %q", fsrv.Snapshot.Hash, tc.hash)
			}
			if fsrv.Snapshot.FollowSymlinks != tc.follow {
				t.Errorf("follow: got %v want %v", fsrv.Snapshot.FollowSymlinks, tc.follow)
			}
			if fsrv.Snapshot.RevealSymlinks != tc.reveal {
				t.Errorf("reveal: got %v want %v", fsrv.Snapshot.RevealSymlinks, tc.reveal)
			}
			if fsrv.Snapshot.FileLimit != tc.fileLimit {
				t.Errorf("file_limit: got %d want %d", fsrv.Snapshot.FileLimit, tc.fileLimit)
			}
		})
	}

	// invalid hash is caught at provision time
	fsrv := &FileServer{Snapshot: &Snapshot{Hash: "bogus"}}
	ctx, _ := caddy.NewContext(caddy.Context{Context: context.Background()})
	if err := fsrv.Provision(ctx); err == nil {
		t.Fatal("expected provision error for invalid hash")
	}
}

func TestSnapshotSpoolOverflow(t *testing.T) {
	spool := newSnapshotSpool()
	payload := bytes.Repeat([]byte("x"), snapshotMemoryLimit+1024)
	if _, err := spool.Write(payload); err != nil {
		t.Fatal(err)
	}
	if spool.f == nil {
		t.Fatal("expected spool to overflow into a temporary file")
	}
	rs, cleanup, err := spool.reader()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("spooled content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	// cleanup after reader ownership transfer must be a safe no-op
	spool.cleanup()
	spool.cleanup()
}

func TestSnapshotEmptyReadSeeker(t *testing.T) {
	e := &emptyReadSeeker{size: 100}
	pos, err := e.Seek(0, io.SeekEnd)
	if err != nil || pos != 100 {
		t.Fatalf("seek end: pos=%d err=%v", pos, err)
	}
	pos, err = e.Seek(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Fatalf("seek start: pos=%d err=%v", pos, err)
	}
	buf := make([]byte, 4)
	if n, err := e.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
}
