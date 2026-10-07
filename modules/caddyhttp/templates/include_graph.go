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
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/caddyserver/caddy/v2"
)

const (
	compiledTemplateFuncVersion = "caddy-templates-funcs-v1"

	defaultCacheCapacity   int64 = 1024
	defaultMaxFileSize     int64 = 1 << 20
	defaultMaxIncludeDepth       = 32
	defaultMaxExpansion    int64 = 10 << 20
	defaultCacheTTL              = time.Hour

	evictionPolicyLRU    = "lru"
	evictionPolicyRandom = "random"
)

var (
	errIncludeCycle     = errors.New("template include cycle")
	errMaxIncludeDepth  = errors.New("template include depth exceeded")
	errMaxFileSize      = errors.New("template file size exceeded")
	errMaxExpansionSize = errors.New("template expansion size exceeded")
)

// IncludeGraphPolicy controls how templates may include and import one another.
type IncludeGraphPolicy struct {
	// CompiledCache enables caching parsed template files.
	CompiledCache *bool `json:"compiled_cache,omitempty"`

	// CacheCapacity is the maximum number of parsed template entries.
	CacheCapacity int64 `json:"cache_capacity,omitempty"`

	// MaxFileSize is the maximum size of a template file in bytes.
	MaxFileSize int64 `json:"max_file_size,omitempty"`

	// MaxIncludeDepth is the maximum number of include, import, and
	// httpInclude edges in one render.
	MaxIncludeDepth int `json:"max_include_depth,omitempty"`

	// MaxExpansionBytes is the maximum number of bytes expanded in one render.
	MaxExpansionBytes int64 `json:"max_expansion_bytes,omitempty"`

	// FileChangeCheck is how long a cached file identity may be used before
	// the file is checked again. Zero checks on every use.
	FileChangeCheck caddy.Duration `json:"file_change_check,omitempty"`

	// CacheTTL is the maximum lifetime of a parsed template entry.
	CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`

	// EvictionPolicy selects the strategy used when the cache is full.
	EvictionPolicy string `json:"eviction_policy,omitempty"`
}

func (p *IncludeGraphPolicy) provision(funcVersion string) (*templateCache, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if p.CompiledCache == nil {
		compiledCache := true
		p.CompiledCache = &compiledCache
	}
	if p.CacheCapacity == 0 {
		p.CacheCapacity = defaultCacheCapacity
	}
	if p.MaxFileSize == 0 {
		p.MaxFileSize = defaultMaxFileSize
	}
	if p.MaxIncludeDepth == 0 {
		p.MaxIncludeDepth = defaultMaxIncludeDepth
	}
	if p.MaxExpansionBytes == 0 {
		p.MaxExpansionBytes = defaultMaxExpansion
	}
	if p.CacheTTL == 0 {
		p.CacheTTL = caddy.Duration(defaultCacheTTL)
	}
	if p.EvictionPolicy == "" {
		p.EvictionPolicy = evictionPolicyLRU
	}
	if !*p.CompiledCache {
		return nil, nil
	}
	return newTemplateCache(p, funcVersion), nil
}

func (p *IncludeGraphPolicy) validate() error {
	if p.CacheCapacity < -1 {
		return fmt.Errorf("cache_capacity must be positive or -1 for unlimited, got %d", p.CacheCapacity)
	}
	if p.MaxFileSize < -1 {
		return fmt.Errorf("max_file_size must be positive or -1 for unlimited, got %d", p.MaxFileSize)
	}
	if p.MaxIncludeDepth < 0 {
		return fmt.Errorf("max_include_depth must be positive, got %d", p.MaxIncludeDepth)
	}
	if p.MaxExpansionBytes < -1 {
		return fmt.Errorf("max_expansion_bytes must be positive or -1 for unlimited, got %d", p.MaxExpansionBytes)
	}
	if p.FileChangeCheck < -1 {
		return fmt.Errorf("file_change_check must be non-negative or -1 to disable, got %s", time.Duration(p.FileChangeCheck))
	}
	if p.CacheTTL < 0 {
		return fmt.Errorf("cache_ttl must be positive, got %s", time.Duration(p.CacheTTL))
	}
	switch p.EvictionPolicy {
	case "", evictionPolicyLRU, evictionPolicyRandom:
	default:
		return fmt.Errorf("unsupported template cache eviction policy %q", p.EvictionPolicy)
	}
	return nil
}

func templateFunctionsVersion(customFuncs []template.FuncMap) string {
	values := []string{compiledTemplateFuncVersion}
	for i, funcMap := range customFuncs {
		names := make([]string, 0, len(funcMap))
		for name := range funcMap {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pointer := uintptr(0)
			if value := reflect.ValueOf(funcMap[name]); value.Kind() == reflect.Func {
				pointer = value.Pointer()
			}
			values = append(values, fmt.Sprintf("%d:%s:%x", i, name, pointer))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(sum[:])
}

type templateCacheKey struct {
	root        string
	path        string
	delimiters  string
	funcVersion string
	size        int64
	mode        fs.FileMode
	modTime     time.Time
}

type compiledTemplateEntry struct {
	rootName  string
	trees     map[string]*parse.Tree
	info      fs.FileInfo
	createdAt time.Time
	lastCheck time.Time
	element   *list.Element
}

type pendingTemplateEntry struct {
	rootName string
	trees    map[string]*parse.Tree
	info     fs.FileInfo
}

type templateCache struct {
	mu          sync.Mutex
	entries     map[templateCacheKey]*compiledTemplateEntry
	versions    map[templateCacheVersionKey]templateCacheKey
	order       *list.List
	capacity    int64
	ttl         time.Duration
	fileCheck   time.Duration
	eviction    string
	funcVersion string
	hits        int64
	closed      bool
}

func newTemplateCache(policy *IncludeGraphPolicy, funcVersion string) *templateCache {
	return &templateCache{
		entries:     make(map[templateCacheKey]*compiledTemplateEntry),
		versions:    make(map[templateCacheVersionKey]templateCacheKey),
		order:       list.New(),
		capacity:    policy.CacheCapacity,
		ttl:         time.Duration(policy.CacheTTL),
		fileCheck:   time.Duration(policy.FileChangeCheck),
		eviction:    policy.EvictionPolicy,
		funcVersion: funcVersion,
	}
}

type templateCacheVersionKey struct {
	root        string
	path        string
	delimiters  string
	funcVersion string
}

func (c *templateCache) get(key templateCacheKey, info fs.FileInfo, now time.Time) (*compiledTemplateEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}
	entry, ok := c.entries[key]
	if !ok {
		c.deleteEntryVersionLocked(key)
		return nil, false
	}
	if now.Sub(entry.createdAt) >= c.ttl {
		c.deleteLocked(key, entry)
		return nil, false
	}
	if c.fileCheck >= 0 && now.Sub(entry.lastCheck) >= c.fileCheck {
		if !sameTemplateFile(entry.info, info) {
			c.deleteLocked(key, entry)
			return nil, false
		}
		entry.lastCheck = now
	}
	if c.eviction == evictionPolicyLRU && entry.element != nil {
		c.order.MoveToFront(entry.element)
	}
	c.hits++
	return entry, true
}

func (c *templateCache) peek(version templateCacheVersionKey, now time.Time) (*compiledTemplateEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}
	key, ok := c.versions[version]
	if !ok {
		return nil, false
	}
	entry, ok := c.entries[key]
	if !ok {
		delete(c.versions, version)
		return nil, false
	}
	if now.Sub(entry.createdAt) >= c.ttl {
		c.deleteLocked(key, entry)
		return nil, false
	}
	if c.fileCheck < 0 || (c.fileCheck > 0 && now.Sub(entry.lastCheck) < c.fileCheck) {
		if c.eviction == evictionPolicyLRU && entry.element != nil {
			c.order.MoveToFront(entry.element)
		}
		c.hits++
		return entry, true
	}
	return nil, false
}

func (c *templateCache) put(key templateCacheKey, rootName string, trees map[string]*parse.Tree, info fs.FileInfo, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if old, ok := c.entries[key]; ok {
		c.deleteLocked(key, old)
	}
	if c.capacity > 0 {
		for int64(len(c.entries)) >= c.capacity {
			c.evictLocked()
		}
	}
	entry := &compiledTemplateEntry{
		rootName:  rootName,
		trees:     trees,
		info:      info,
		createdAt: now,
		lastCheck: now,
	}
	if c.eviction == evictionPolicyLRU {
		entry.element = c.order.PushFront(key)
	}
	c.entries[key] = entry
	c.versions[templateCacheVersionKey{
		root:        key.root,
		path:        key.path,
		delimiters:  key.delimiters,
		funcVersion: key.funcVersion,
	}] = key
}

func (c *templateCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	clear(c.entries)
	clear(c.versions)
	c.order.Init()
}

func (c *templateCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *templateCache) hitCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func (c *templateCache) deleteLocked(key templateCacheKey, entry *compiledTemplateEntry) {
	delete(c.entries, key)
	version := templateCacheVersionKey{
		root:        key.root,
		path:        key.path,
		delimiters:  key.delimiters,
		funcVersion: key.funcVersion,
	}
	if current, ok := c.versions[version]; ok && current == key {
		delete(c.versions, version)
	}
	if entry.element != nil {
		c.order.Remove(entry.element)
	}
}

func (c *templateCache) deleteEntryVersionLocked(key templateCacheKey) {
	version := templateCacheVersionKey{
		root:        key.root,
		path:        key.path,
		delimiters:  key.delimiters,
		funcVersion: key.funcVersion,
	}
	oldKey, ok := c.versions[version]
	if !ok {
		return
	}
	if entry, ok := c.entries[oldKey]; ok {
		c.deleteLocked(oldKey, entry)
	}
}

func (c *templateCache) evictLocked() {
	if c.eviction == evictionPolicyRandom {
		index := rand.IntN(len(c.entries))
		for key, entry := range c.entries {
			if index == 0 {
				c.deleteLocked(key, entry)
				return
			}
			index--
		}
	}
	if back := c.order.Back(); back != nil {
		key := back.Value.(templateCacheKey)
		if entry, ok := c.entries[key]; ok {
			c.deleteLocked(key, entry)
		}
	}
}

func sameTemplateFile(cached, current fs.FileInfo) bool {
	if os.SameFile(cached, current) {
		return true
	}
	return cached.Name() == current.Name() &&
		cached.Size() == current.Size() &&
		cached.Mode() == current.Mode() &&
		cached.ModTime().Equal(current.ModTime()) &&
		cached.IsDir() == current.IsDir()
}

func cloneTemplateTrees(trees map[string]*parse.Tree) map[string]*parse.Tree {
	cloned := make(map[string]*parse.Tree, len(trees))
	for name, tree := range trees {
		cloned[name] = tree.Copy()
	}
	return cloned
}

func rootFileSystemKey(root http.FileSystem) string {
	if dir, ok := root.(http.Dir); ok {
		return "dir:" + string(dir)
	}
	return fmt.Sprintf("%T:%p", root, root)
}

func cleanTemplatePath(filename string) string {
	return path.Clean("/" + filename)
}

type renderStateContextKeyType struct{}

var renderStateContextKey renderStateContextKeyType

type activeTemplateNode struct {
	key  string
	root bool
}

type templateRenderState struct {
	mu               sync.Mutex
	active           []activeTemplateNode
	activeNodes      map[string]struct{}
	depth            int
	expanded         int64
	childBytes       int64
	pendingTemplates map[templateCacheKey]pendingTemplateEntry
	pendingVersions  map[templateCacheVersionKey]templateCacheKey
	validatedImports map[string]int
}

func newTemplateRenderState() *templateRenderState {
	return &templateRenderState{
		activeNodes:      make(map[string]struct{}),
		pendingTemplates: make(map[templateCacheKey]pendingTemplateEntry),
		pendingVersions:  make(map[templateCacheVersionKey]templateCacheKey),
		validatedImports: make(map[string]int),
	}
}

func (s *templateRenderState) pushRoot(key string) func() {
	s.mu.Lock()
	node := activeTemplateNode{key: key, root: true}
	s.active = append(s.active, node)
	s.activeNodes[key] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active = s.active[:len(s.active)-1]
		delete(s.activeNodes, key)
		s.mu.Unlock()
	}
}

func (s *templateRenderState) enter(key string, maxDepth int) (func(), error) {
	s.mu.Lock()
	if _, ok := s.activeNodes[key]; ok {
		s.mu.Unlock()
		return nil, errIncludeCycle
	}
	if s.depth >= maxDepth {
		s.mu.Unlock()
		return nil, errMaxIncludeDepth
	}
	s.active = append(s.active, activeTemplateNode{key: key})
	s.activeNodes[key] = struct{}{}
	s.depth++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active = s.active[:len(s.active)-1]
		delete(s.activeNodes, key)
		s.depth--
		s.mu.Unlock()
	}, nil
}

func (s *templateRenderState) hasActiveNodes() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active) > 0
}

func (s *templateRenderState) currentDepth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.depth
}

func (s *templateRenderState) claimExpansion(n int64, limit int64) error {
	if limit < 0 {
		s.mu.Lock()
		s.expanded += n
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expanded+n > limit {
		return errMaxExpansionSize
	}
	s.expanded += n
	return nil
}

func (s *templateRenderState) expandedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expanded
}

func (s *templateRenderState) currentChildBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childBytes
}

func (s *templateRenderState) addChildOutput(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.childBytes += int64(n)
	s.mu.Unlock()
}

func (s *templateRenderState) setExpanded(value int64) {
	s.mu.Lock()
	s.expanded = value
	s.mu.Unlock()
}

func (s *templateRenderState) pendingTemplateVersion(version templateCacheVersionKey) (pendingTemplateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.pendingVersions[version]
	if !ok {
		return pendingTemplateEntry{}, false
	}
	entry, ok := s.pendingTemplates[key]
	return entry, ok
}

func (s *templateRenderState) storePendingTemplate(key templateCacheKey, entry pendingTemplateEntry) {
	s.mu.Lock()
	s.pendingTemplates[key] = entry
	s.pendingVersions[templateCacheVersionKey{
		root:        key.root,
		path:        key.path,
		delimiters:  key.delimiters,
		funcVersion: key.funcVersion,
	}] = key
	s.mu.Unlock()
}

func (s *templateRenderState) promotePendingTemplates(cache *templateCache, now time.Time) {
	s.mu.Lock()
	entries := make([]struct {
		key   templateCacheKey
		entry pendingTemplateEntry
	}, 0, len(s.pendingTemplates))
	for key, entry := range s.pendingTemplates {
		entries = append(entries, struct {
			key   templateCacheKey
			entry pendingTemplateEntry
		}{key: key, entry: entry})
	}
	s.pendingTemplates = make(map[templateCacheKey]pendingTemplateEntry)
	s.pendingVersions = make(map[templateCacheVersionKey]templateCacheKey)
	s.mu.Unlock()

	if cache == nil {
		return
	}
	for _, item := range entries {
		cache.put(item.key, item.entry.rootName, cloneTemplateTrees(item.entry.trees), item.entry.info, now)
	}
}

func (s *templateRenderState) importValidationDepth(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	depth, ok := s.validatedImports[key]
	return depth, ok
}

func (s *templateRenderState) markImportValidated(key string, depth int) {
	s.mu.Lock()
	if previous, ok := s.validatedImports[key]; ok && depth > previous {
		depth = previous
	}
	s.validatedImports[key] = depth
	s.mu.Unlock()
}

type expansionLimitedWriter struct {
	buf               *bytes.Buffer
	state             *templateRenderState
	limit             int64
	childBytesAtStart int64
	lastDirectBytes   int64
}

func (w *expansionLimitedWriter) Write(p []byte) (int, error) {
	childBytes := w.state.currentChildBytes() - w.childBytesAtStart
	direct := int64(w.buf.Len()) + int64(len(p)) - childBytes
	if direct < 0 {
		direct = 0
	}
	delta := direct - w.lastDirectBytes
	if delta > 0 {
		if err := w.state.claimExpansion(delta, w.limit); err != nil {
			return 0, err
		}
	}
	w.lastDirectBytes = direct
	return w.buf.Write(p)
}
