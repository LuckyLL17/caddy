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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

const (
	// snapshotMediaType is the media type served (and accepted for
	// content negotiation) by atomic directory snapshots.
	snapshotMediaType = "application/vnd.caddy.snapshot+json"

	// snapshotManifestVersion is the version of the manifest format
	// emitted by this implementation. Bump it when the on-wire format
	// changes in an incompatible way.
	snapshotManifestVersion = 1

	// Snapshot content digest modes.
	snapshotHashMetadata = "metadata" // default: derive per-file validators from size and mod time only
	snapshotHashSHA256   = "sha256"   // additionally stream each file's contents through SHA-256

	// snapshotMemoryLimit bounds how much manifest data is buffered in
	// memory per request; larger manifests are spooled to a temporary
	// file. File contents are never buffered, only manifest records.
	snapshotMemoryLimit = 4 << 20

	// snapshotMaxDepth bounds recursion depth when following symlinks,
	// as a backstop against filesystems that cannot be canonicalized.
	snapshotMaxDepth = 64

	// snapshotReadDirBatch is how many directory entries are read per
	// ReadDir call while walking a tree.
	snapshotReadDirBatch = defaultDirEntryLimit

	// snapshotHashBufSize is the chunk size used when hashing file
	// contents; the same size used by io.Copy.
	snapshotHashBufSize = 32 * 1024
)

// Snapshot configures atomic directory snapshots. A snapshot is a
// machine-consumable manifest of a directory and all of its descendants,
// generated with a single filesystem walk per request. It reuses the file
// server's configured fs.FS, hide list, symlink handling and HTTP
// conditional request semantics, and never keeps directory contents in
// memory beyond the lifetime of a request.
//
// A snapshot of a directory is requested with the `?snapshot` query
// parameter, or by sending an Accept header containing
// `application/vnd.caddy.snapshot+json`. The response is a JSON document
// with a strong, content-derived ETag and a Last-Modified header, so
// If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since and
// Range work exactly as they do for regular static file responses.
//
// Each file entry carries the same ETag the file server would return when
// serving that file, allowing clients to fetch the manifest once and then
// verify or conditionally download individual files. The manifest itself
// is self-consistent: its ETag is computed over the exact records that
// make up the response body during the same walk.
type Snapshot struct {
	// Hash selects how file entries are digested. With "metadata" (the
	// default), only file metadata is read and per-file validators are
	// derived from size and modification time. With "sha256", each
	// file's contents are additionally streamed through SHA-256 and the
	// result is included as a content digest in each entry. File
	// contents are streamed, not buffered, either way.
	Hash string `json:"hash,omitempty"`

	// FollowSymlinks causes symlinked directories to be traversed and
	// their contents included in the manifest. By default symlinks are
	// reported as symlink entries but not followed, which matches the
	// behavior of fs.WalkDir and prevents loops or escaping the site
	// root. Symbolic link cycles are skipped even when enabled.
	FollowSymlinks bool `json:"follow_symlinks,omitempty"`

	// RevealSymlinks includes the target of each symbolic link in the
	// manifest entry. Uses the same semantics as browse's
	// reveal_symlinks option.
	RevealSymlinks bool `json:"reveal_symlinks,omitempty"`

	// FileLimit caps the number of entries read from any single
	// directory. A directory with more entries causes the snapshot to
	// fail rather than producing a truncated manifest. Default 0 means
	// no limit.
	FileLimit int `json:"file_limit,omitempty"`
}

// snapshotEntry is a single record of a directory snapshot. Paths are
// slash-separated and relative to the snapshotted directory; the root
// directory itself is represented by ".".
type snapshotEntry struct {
	// Path is the slash-separated path of the entry relative to the
	// snapshotted directory ("." for the root directory itself).
	Path string `json:"path"`

	// Type is "file", "dir" or "symlink".
	Type string `json:"type"`

	// Size is the size in bytes. For symlinks it reflects the size of
	// the target when it can be stated, mirroring directory listings.
	Size int64 `json:"size"`

	// Mode is the file's mode and type bits, as returned by the
	// filesystem (os.FileMode).
	Mode uint32 `json:"mode"`

	// ModTime is the modification time in UTC.
	ModTime time.Time `json:"mod_time"`

	// ETag is the validator the file server returns when serving this
	// file, allowing conditional downloads of individual entries.
	ETag string `json:"etag,omitempty"`

	// Digest is the content digest ("sha256:<hex>") when content
	// hashing is enabled.
	Digest string `json:"digest,omitempty"`

	// SymlinkPath is the raw target of a symbolic link when symlink
	// revealing is enabled and the target can be read.
	SymlinkPath string `json:"symlink_path,omitempty"`

	// TargetType reports the resolved type of a symlink target:
	// "file", "dir", or "broken" when the target cannot be stated.
	TargetType string `json:"target_type,omitempty"`
}

// snapshotRequested reports whether r asks for a directory snapshot.
func (fsrv *FileServer) snapshotRequested(r *http.Request) bool {
	if fsrv.Snapshot == nil {
		return false
	}
	if _, ok := r.URL.Query()["snapshot"]; ok {
		return true
	}
	return strings.Contains(strings.ToLower(strings.Join(r.Header["Accept"], ",")), snapshotMediaType)
}

// serveSnapshot writes an atomic manifest of dirPath to w. rootInfo is
// the directory's file info from the request's initial stat.
func (fsrv *FileServer) serveSnapshot(fileSystem fs.FS, fsName, dirPath string, rootInfo fs.FileInfo, w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Relative links in manifests (and the trailing-slash conventions
	// of the underlying file server) only work when directory URLs end
	// in a slash; apply the same redirect as directory browsing.
	origReq := r.Context().Value(caddyhttp.OriginalRequestCtxKey).(http.Request)
	if r.URL.Path == "" || path.Base(origReq.URL.Path) == path.Base(r.URL.Path) {
		if !strings.HasSuffix(origReq.URL.Path, "/") {
			if c := fsrv.logger.Check(zapcore.DebugLevel, "redirecting snapshot request to trailing slash"); c != nil {
				c.Write(zap.String("request_path", origReq.URL.Path))
			}
			return redirect(w, r, origReq.URL.Path+"/")
		}
	}

	// manifests are safe to produce for GET and HEAD only, like files
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if _, ok := r.Context().Value(caddyhttp.ErrorCtxKey).(error); !ok {
			w.Header().Set("Allow", "GET, HEAD")
			return caddyhttp.Error(http.StatusMethodNotAllowed, nil)
		}
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// One walk builds the manifest and its strong validator together,
	// so the body and ETag can never disagree, even if the tree changes
	// while the walk is in progress. Output is streamed into a bounded
	// spool (memory, then a temporary file) rather than an in-memory
	// tree that lives for the lifetime of the server.
	spool := newSnapshotSpool()
	defer spool.cleanup()

	var out io.Writer = spool
	var contentLen counter
	if r.Method == http.MethodHead {
		out = &contentLen
	}

	digestHex, lastModified, entryCount, err := fsrv.writeSnapshot(r.Context(), fileSystem, fsName, dirPath, r.URL.Path, rootInfo, repl, out)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return caddyhttp.Error(http.StatusForbidden, err)
	case errors.Is(err, fs.ErrNotExist):
		return fsrv.notFound(w, r, next)
	case err != nil:
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}

	hdr := w.Header()
	hdr.Set("Content-Type", snapshotMediaType+"; charset=utf-8")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Add("Vary", "Accept")
	hdr.Set("ETag", `"`+digestHex+`"`)

	var content io.ReadSeeker
	if r.Method == http.MethodHead {
		content = &emptyReadSeeker{size: int64(contentLen)}
	} else {
		var cleanup func()
		content, cleanup, err = spool.reader()
		if err != nil {
			return caddyhttp.Error(http.StatusInternalServerError, err)
		}
		defer cleanup()
	}

	// Delegate all conditional request handling (If-Match, If-None-Match,
	// If-Modified-Since, If-Unmodified-Since, If-Range) and Range support
	// to the standard library, exactly as ordinary file responses do.
	modTime := lastModified
	if !usefulModTime(modTime) {
		modTime = time.Time{}
	}
	http.ServeContent(w, r, "snapshot.json", modTime, content)

	if c := fsrv.logger.Check(zapcore.DebugLevel, "served directory snapshot"); c != nil {
		c.Write(
			zap.String("path", dirPath),
			zap.Int("entry_count", entryCount),
			zap.String("digest", digestHex),
		)
	}

	return nil
}

// writeSnapshot performs the single manifest walk, writing the JSON
// document to out while simultaneously computing the tree digest. It
// returns the digest hex, the most recent modification time observed,
// and the number of entries emitted (including the root directory).
func (fsrv *FileServer) writeSnapshot(ctx context.Context, fileSystem fs.FS, fsName, dirPath, urlPath string, rootInfo fs.FileInfo, repl *caddy.Replacer, out io.Writer) (string, time.Time, int, error) {
	filesToHide := fsrv.transformHidePaths(repl)

	treeHash := sha256.New()
	enc := json.NewEncoder(out)

	// clean the path but keep the trailing slash that marks a directory
	manifestPath := path.Clean(urlPath)
	if strings.HasSuffix(urlPath, "/") && manifestPath != "/" {
		manifestPath += "/"
	}
	escapedPath, err := json.Marshal(manifestPath)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	if _, err := fmt.Fprintf(out, `{"version":%d,"path":%s,"hash":%q,"entries":[`,
		snapshotManifestVersion, escapedPath, fsrv.Snapshot.Hash); err != nil {
		return "", time.Time{}, 0, err
	}

	var lastModified time.Time
	entryCount := 0
	first := true

	// emit serializes one record: the same canonical bytes update the
	// tree digest and are the basis of the JSON object that is written,
	// so digest and body are always in agreement.
	emit := func(relPath, entryType string, info, targetInfo fs.FileInfo, symlinkTarget, contentDigest string) error {
		modTime := info.ModTime().UTC()
		if modTime.After(lastModified) {
			lastModified = modTime
		}

		// size and ETag must describe what GET would serve: for symlinks
		// that means the target, which is also why a broken link (no
		// target) carries neither size override nor a validator
		size := info.Size()
		etag := ""
		switch {
		case entryType == "symlink" && targetInfo != nil:
			size = targetInfo.Size()
			if !targetInfo.IsDir() {
				etag = calculateEtag(targetInfo)
			}
		case entryType != "symlink" && !info.IsDir():
			etag = calculateEtag(info)
		}

		fmt.Fprintf(treeHash, "%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\n",
			relPath, entryType, uint32(info.Mode()), size, modTime.UnixNano(), etag, contentDigest)

		entry := snapshotEntry{
			Path:        relPath,
			Type:        entryType,
			Size:        size,
			Mode:        uint32(info.Mode()),
			ModTime:     modTime,
			ETag:        etag,
			Digest:      contentDigest,
			SymlinkPath: symlinkTarget,
		}
		if entryType == "symlink" {
			switch {
			case targetInfo == nil:
				entry.TargetType = "broken"
			case targetInfo.IsDir():
				entry.TargetType = "dir"
			default:
				entry.TargetType = "file"
			}
		}

		if !first {
			if _, err := io.WriteString(out, ","); err != nil {
				return err
			}
		}
		first = false
		entryCount++
		return enc.Encode(entry)
	}

	hashBuf := make([]byte, snapshotHashBufSize)
	hashContent := func(fsPath string) (string, error) {
		f, err := fileSystem.Open(fsPath)
		if err != nil {
			return "", fsrv.mapDirOpenError(fileSystem, err, fsPath)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.CopyBuffer(h, f, hashBuf); err != nil {
			return "", err
		}
		return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
	}

	visited := make(map[string]bool)

	var walk func(fsDir, relDir string, depth int, enteredViaSymlink bool) error
	walk = func(fsDir, relDir string, depth int, enteredViaSymlink bool) error {
		if depth > snapshotMaxDepth {
			return fmt.Errorf("%w: more than %d levels", errSnapshotDepth, snapshotMaxDepth)
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if enteredViaSymlink && fsName == "" {
			// Custom filesystems cannot be canonicalized against the
			// host filesystem; the depth limit is their loop guard.
			if resolved, err := filepath.EvalSymlinks(fsDir); err == nil {
				if visited[resolved] {
					return nil
				}
				visited[resolved] = true
			}
		}

		f, err := fileSystem.Open(fsDir)
		if err != nil {
			return fsrv.mapDirOpenError(fileSystem, err, fsDir)
		}
		defer f.Close()

		rd, ok := f.(fs.ReadDirFile)
		if !ok {
			return fmt.Errorf("%s is not a readable directory", fsDir)
		}

		var entries []fs.DirEntry
		for {
			batch, err := rd.ReadDir(snapshotReadDirBatch)
			entries = append(entries, batch...)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if fsrv.Snapshot.FileLimit > 0 && len(entries) > fsrv.Snapshot.FileLimit {
				return fmt.Errorf("%w: %s has more than %d entries", errSnapshotLimit, fsDir, fsrv.Snapshot.FileLimit)
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})

		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}

			name := ent.Name()
			childPath := caddyhttp.SanitizedPathJoin(fsDir, name)
			if fileHidden(childPath, filesToHide) {
				continue
			}

			info, err := ent.Info()
			if err != nil {
				return err
			}

			relPath := name
			if relDir != "" {
				relPath = relDir + "/" + name
			}

			isLink := isSymlink(info)
			var targetInfo fs.FileInfo
			if isLink {
				// a missing target is an ordinary case for a broken
				// link; leave targetInfo nil and report it as broken
				targetInfo, _ = fs.Stat(fileSystem, childPath)
			}

			isDir := ent.IsDir() || (targetInfo != nil && targetInfo.IsDir())
			entryType := "file"
			switch {
			case isLink:
				entryType = "symlink"
			case isDir:
				entryType = "dir"
			}

			symlinkTarget := ""
			if isLink && fsrv.Snapshot.RevealSymlinks {
				if t, err := os.Readlink(childPath); err == nil {
					symlinkTarget = t
				}
			}

			contentDigest := ""
			if fsrv.Snapshot.Hash == snapshotHashSHA256 && !isDir && (!isLink || targetInfo != nil) {
				contentDigest, err = hashContent(childPath)
				if err != nil {
					return err
				}
			}

			if err := emit(relPath, entryType, info, targetInfo, symlinkTarget, contentDigest); err != nil {
				return err
			}

			if isDir && (!isLink || fsrv.Snapshot.FollowSymlinks) {
				if err := walk(childPath, relPath, depth+1, isLink); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// record the root itself first; its modification time changes when
	// entries are added or removed and therefore participates in the
	// digest and Last-Modified value
	if err := emit(".", "dir", rootInfo, nil, "", ""); err != nil {
		return "", time.Time{}, 0, err
	}
	if err := walk(dirPath, "", 0, false); err != nil {
		return "", time.Time{}, 0, err
	}

	if !lastModified.IsZero() {
		escapedModTime, err := json.Marshal(lastModified)
		if err != nil {
			return "", time.Time{}, 0, err
		}
		if _, err := fmt.Fprintf(out, `],"last_modified":%s`, escapedModTime); err != nil {
			return "", time.Time{}, 0, err
		}
	} else {
		if _, err := io.WriteString(out, "]"); err != nil {
			return "", time.Time{}, 0, err
		}
	}

	digestHex := hex.EncodeToString(treeHash.Sum(nil))
	if _, err := fmt.Fprintf(out, `,"entry_count":%d,"digest":%q}`, entryCount, digestHex); err != nil {
		return "", time.Time{}, 0, err
	}

	return digestHex, lastModified.UTC(), entryCount, nil
}

var (
	errSnapshotDepth = errors.New("snapshot depth limit exceeded")
	errSnapshotLimit = errors.New("snapshot file limit exceeded")
)

// counter is an io.Writer that only counts bytes, used for HEAD requests
// where the body is never sent but Content-Length still has to be exact.
type counter int64

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}

// emptyReadSeeker is a zero-byte ReadSeeker with a reported size.
// http.ServeContent only seeks on HEAD responses and never reads.
type emptyReadSeeker struct {
	size int64
	pos  int64
}

func (e *emptyReadSeeker) Read([]byte) (int, error) { return 0, io.EOF }

func (e *emptyReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		e.pos = offset
	case io.SeekCurrent:
		e.pos += offset
	case io.SeekEnd:
		e.pos = e.size + offset
	}
	if e.pos < 0 {
		return 0, errors.New("negative seek position")
	}
	return e.pos, nil
}

// snapshotSpool collects a manifest body with bounded memory use: small
// documents stay in a buffer, larger ones spill to a temporary file that
// is removed as soon as the response has been written.
type snapshotSpool struct {
	buf bytes.Buffer
	f   *os.File
	n   int64
}

func newSnapshotSpool() *snapshotSpool {
	return &snapshotSpool{}
}

func (s *snapshotSpool) Write(p []byte) (int, error) {
	if s.f == nil && s.buf.Len()+len(p) > snapshotMemoryLimit {
		f, err := os.CreateTemp("", "caddy-snapshot-*.json")
		if err != nil {
			return 0, err
		}
		if _, err := s.buf.WriteTo(f); err != nil {
			f.Close()
			os.Remove(f.Name())
			return 0, err
		}
		s.buf = bytes.Buffer{}
		s.f = f
	}
	if s.f != nil {
		n, err := s.f.Write(p)
		s.n += int64(n)
		return n, err
	}
	n, err := s.buf.Write(p)
	s.n += int64(n)
	return n, err
}

// reader returns the spooled content seeked back to the beginning along
// with a cleanup function. Ownership of a spooled temporary file is
// transferred to the returned cleanup.
func (s *snapshotSpool) reader() (io.ReadSeeker, func(), error) {
	if s.f != nil {
		f := s.f
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, nil, err
		}
		s.f = nil
		return f, func() {
			f.Close()
			os.Remove(f.Name())
		}, nil
	}
	return bytes.NewReader(s.buf.Bytes()), func() {}, nil
}

// cleanup removes any spooled temporary file still held by the spool.
func (s *snapshotSpool) cleanup() {
	if s.f != nil {
		s.f.Close()
		os.Remove(s.f.Name())
		s.f = nil
	}
}
