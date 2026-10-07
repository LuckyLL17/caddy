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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/Masterminds/sprig/v3"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/dustin/go-humanize"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// TemplateContext is the TemplateContext with which HTTP templates are executed.
type TemplateContext struct {
	Root        http.FileSystem
	Req         *http.Request
	Args        []any // defined by arguments to funcInclude
	RespHeader  WrappedHeader
	CustomFuncs []template.FuncMap // functions added by plugins

	config *Templates
	tpl    *template.Template
	state  *templateRenderState
}

// NewTemplate returns a new template intended to be evaluated with this
// context, as it is initialized with configuration from this context.
func (c *TemplateContext) NewTemplate(tplName string) *template.Template {
	c.tpl = template.New(tplName).Option("missingkey=zero")

	// customize delimiters, if applicable
	if c.config != nil && len(c.config.Delimiters) == 2 {
		c.tpl.Delims(c.config.Delimiters[0], c.config.Delimiters[1])
	}

	// add sprig library
	c.tpl.Funcs(sprigFuncMap)

	// add all custom functions
	for _, funcMap := range c.CustomFuncs {
		c.tpl.Funcs(funcMap)
	}

	// add our own library
	c.tpl.Funcs(c.coreTemplateFuncs())
	return c.tpl
}

func (c *TemplateContext) coreTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"include":          c.funcInclude,
		"readFile":         c.funcReadFile,
		"import":           c.funcImport,
		"httpInclude":      c.funcHTTPInclude,
		"stripHTML":        c.funcStripHTML,
		"markdown":         c.funcMarkdown,
		"splitFrontMatter": c.funcSplitFrontMatter,
		"listFiles":        c.funcListFiles,
		"fileStat":         c.funcFileStat,
		"env":              c.funcEnv,
		"placeholder":      c.funcPlaceholder,
		"ph":               c.funcPlaceholder,
		"fileExists":       c.funcFileExists,
		"httpError":        c.funcHTTPError,
		"humanize":         c.funcHumanize,
		"maybe":            c.funcMaybe,
		"pathEscape":       url.PathEscape,
	}
}

// OriginalReq returns the original, unmodified, un-rewritten request as
// it originally came in over the wire.
func (c TemplateContext) OriginalReq() http.Request {
	or, _ := c.Req.Context().Value(caddyhttp.OriginalRequestCtxKey).(http.Request)
	return or
}

// funcInclude returns the contents of filename relative to the site root and renders it in place.
// Note that included files are NOT escaped, so you should only include
// trusted files. If it is not trusted, be sure to use escaping functions
// in your template.
func (c *TemplateContext) funcInclude(filename string, args ...any) (string, error) {
	if c.policy() != nil {
		return c.includeManaged(filename, args...)
	}

	bodyBuf := bufPool.Get().(*bytes.Buffer)
	bodyBuf.Reset()
	defer bufPool.Put(bodyBuf)

	err := c.readFileToBuffer(filename, bodyBuf)
	if err != nil {
		return "", err
	}

	c.Args = args

	err = c.executeTemplateInBuffer(filename, bodyBuf)
	if err != nil {
		return "", err
	}

	return bodyBuf.String(), nil
}

// funcReadFile returns the contents of a filename relative to the site root.
// Note that included files are NOT escaped, so you should only include
// trusted files. If it is not trusted, be sure to use escaping functions
// in your template.
func (c *TemplateContext) funcReadFile(filename string) (string, error) {
	if c.policy() != nil {
		return c.readFileManaged(filename)
	}

	bodyBuf := bufPool.Get().(*bytes.Buffer)
	bodyBuf.Reset()
	defer bufPool.Put(bodyBuf)

	err := c.readFileToBuffer(filename, bodyBuf)
	if err != nil {
		return "", err
	}

	return bodyBuf.String(), nil
}

// readFileToBuffer reads a file into a buffer
func (c TemplateContext) readFileToBuffer(filename string, bodyBuf *bytes.Buffer) error {
	if c.Root == nil {
		return fmt.Errorf("root file system not specified")
	}

	file, err := c.Root.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(bodyBuf, file)
	if err != nil {
		return err
	}

	return nil
}

// funcHTTPInclude returns the body of a virtual (lightweight) request
// to the given URI on the same server. Note that included bodies
// are NOT escaped, so you should only include trusted resources.
// If it is not trusted, be sure to use escaping functions yourself.
func (c *TemplateContext) funcHTTPInclude(uri string) (string, error) {
	if c.policy() != nil {
		return c.httpIncludeManaged(uri)
	}

	// prevent virtual request loops by counting how many levels
	// deep we are; and if we get too deep, return an error
	recursionCount := 1
	if numStr := c.Req.Header.Get(recursionPreventionHeader); numStr != "" {
		num, err := strconv.Atoi(numStr)
		if err != nil {
			return "", fmt.Errorf("parsing %s: %v", recursionPreventionHeader, err)
		}
		if num >= 3 {
			return "", fmt.Errorf("virtual request cycle")
		}
		recursionCount = num + 1
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	virtReq, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return "", err
	}
	virtReq.Host = c.Req.Host
	virtReq.RemoteAddr = "127.0.0.1:10000" // https://github.com/caddyserver/caddy/issues/5835
	virtReq.Header = c.Req.Header.Clone()
	virtReq.Header.Set("Accept-Encoding", "identity") // https://github.com/caddyserver/caddy/issues/4352
	virtReq.Trailer = c.Req.Trailer.Clone()
	virtReq.Header.Set(recursionPreventionHeader, strconv.Itoa(recursionCount))

	vrw := &virtualResponseWriter{body: buf, header: make(http.Header)}
	server := c.Req.Context().Value(caddyhttp.ServerCtxKey).(http.Handler)

	server.ServeHTTP(vrw, virtReq)
	if vrw.status >= 400 {
		return "", fmt.Errorf("http %d", vrw.status)
	}

	err = c.executeTemplateInBuffer(uri, buf)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// funcImport parses the filename into the current template stack. The imported
// file will be rendered within the current template by calling {{ block }} or
// {{ template }} from the standard template library. If the imported file has
// no {{ define }} blocks, the name of the import will be the path
func (c *TemplateContext) funcImport(filename string) (string, error) {
	if c.policy() != nil {
		return c.importManaged(filename)
	}

	bodyBuf := bufPool.Get().(*bytes.Buffer)
	bodyBuf.Reset()
	defer bufPool.Put(bodyBuf)

	err := c.readFileToBuffer(filename, bodyBuf)
	if err != nil {
		return "", err
	}

	_, err = c.tpl.Parse(bodyBuf.String())
	if err != nil {
		return "", err
	}
	return "", nil
}

func (c *TemplateContext) executeTemplateInBuffer(tplName string, buf *bytes.Buffer) error {
	if c.policy() != nil {
		return c.executeManagedTemplateInBuffer(tplName, buf)
	}

	c.NewTemplate(tplName)

	_, err := c.tpl.Parse(buf.String())
	if err != nil {
		return err
	}

	buf.Reset() // reuse buffer for output

	return c.tpl.Execute(buf, c)
}

type managedTemplateFile struct {
	rootKey string
	path    string
	content string
	info    fs.FileInfo
}

func (c *TemplateContext) policy() *IncludeGraphPolicy {
	if c.config == nil {
		return nil
	}
	return c.config.IncludeGraph
}

func (c *TemplateContext) ensureRenderState() *templateRenderState {
	if c.state == nil {
		c.state = newTemplateRenderState()
	}
	return c.state
}

func (c *TemplateContext) requestContextErr() error {
	if c.Req == nil || c.Req.Context() == nil {
		return nil
	}
	return c.Req.Context().Err()
}

func (c *TemplateContext) templateDelimiters() (string, string) {
	if c.config != nil && len(c.config.Delimiters) == 2 {
		return c.config.Delimiters[0], c.config.Delimiters[1]
	}
	return "{{", "}}"
}

func (c *TemplateContext) managedTemplateLocation(filename string) (string, string, error) {
	if c.Root == nil {
		return "", "", fmt.Errorf("root file system not specified")
	}
	return rootFileSystemKey(c.Root), cleanTemplatePath(filename), nil
}

func (c *TemplateContext) openManagedTemplateFile(rootKey, filename string) (http.File, fs.FileInfo, error) {
	if err := c.requestContextErr(); err != nil {
		return nil, nil, err
	}
	file, err := c.Root.Open(filename)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s is a directory", filename)
	}
	if maxFileSize := c.policy().MaxFileSize; maxFileSize >= 0 && info.Size() > maxFileSize {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: %s", errMaxFileSize, filename)
	}
	return file, info, nil
}

func (c *TemplateContext) readManagedTemplateFile(file http.File, filename string) (string, error) {
	reader := io.Reader(file)
	maxFileSize := c.policy().MaxFileSize
	if maxFileSize >= 0 {
		reader = io.LimitReader(file, maxFileSize+1)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if maxFileSize >= 0 && int64(len(content)) > maxFileSize {
		return "", fmt.Errorf("%w: %s", errMaxFileSize, filename)
	}
	if err := c.requestContextErr(); err != nil {
		return "", err
	}
	return string(content), nil
}

func (c *TemplateContext) loadManagedTemplateFile(rootKey, filename string) (managedTemplateFile, error) {
	file, info, err := c.openManagedTemplateFile(rootKey, filename)
	if err != nil {
		return managedTemplateFile{}, err
	}
	defer file.Close()
	content, err := c.readManagedTemplateFile(file, filename)
	if err != nil {
		return managedTemplateFile{}, err
	}
	return managedTemplateFile{rootKey: rootKey, path: filename, content: content, info: info}, nil
}

func (c *TemplateContext) cacheKey(rootKey, filename string, info fs.FileInfo) templateCacheKey {
	leftDelim, rightDelim := c.templateDelimiters()
	return templateCacheKey{
		root:        rootKey,
		path:        filename,
		delimiters:  leftDelim + "\x00" + rightDelim,
		funcVersion: c.config.funcVersion,
		size:        info.Size(),
		mode:        info.Mode(),
		modTime:     info.ModTime(),
	}
}

func (c *TemplateContext) cacheKeyFor(file managedTemplateFile) templateCacheKey {
	return c.cacheKey(file.rootKey, file.path, file.info)
}

func (c *TemplateContext) parseManagedTemplate(name, content string) (map[string]*parse.Tree, error) {
	leftDelim, rightDelim := c.templateDelimiters()
	funcMaps := []map[string]any{c.coreTemplateFuncs(), sprigFuncMap}
	for _, funcMap := range c.CustomFuncs {
		funcMaps = append(funcMaps, funcMap)
	}
	return parse.Parse(name, content, leftDelim, rightDelim, funcMaps...)
}

func addParsedTemplateTrees(tpl *template.Template, trees map[string]*parse.Tree, sourceRoot, targetRoot string) error {
	for name, tree := range trees {
		copied := tree.Copy()
		targetName := name
		if name == sourceRoot {
			targetName = targetRoot
			copied.Name = targetRoot
			copied.ParseName = targetRoot
		}
		if _, err := tpl.AddParseTree(targetName, copied); err != nil {
			return err
		}
	}
	return nil
}

func (c *TemplateContext) validateImportGraph(rootKey string, trees map[string]*parse.Tree, stack []string) error {
	for target := range staticTemplateImports(trees) {
		targetPath := cleanTemplatePath(target)
		targetKey := "file:" + rootKey + ":" + targetPath
		for _, active := range stack {
			if active == targetKey {
				return errIncludeCycle
			}
		}
		targetDepth := c.state.currentDepth() + len(stack) + 1
		if targetDepth > c.policy().MaxIncludeDepth {
			return errMaxIncludeDepth
		}
		if validatedDepth, ok := c.state.importValidationDepth(targetKey); ok && targetDepth <= validatedDepth {
			continue
		}
		importedTrees, err := c.managedTemplateTrees(rootKey, targetPath)
		if err != nil {
			return err
		}
		if err := c.validateImportGraph(rootKey, importedTrees, append(stack, targetKey)); err != nil {
			return err
		}
		c.state.markImportValidated(targetKey, targetDepth)
	}
	return nil
}

func staticTemplateImports(trees map[string]*parse.Tree) map[string]struct{} {
	imports := make(map[string]struct{})
	for _, tree := range trees {
		walkTemplateImports(tree.Root, imports)
	}
	return imports
}

func walkTemplateImports(node parse.Node, imports map[string]struct{}) {
	switch node := node.(type) {
	case *parse.ListNode:
		for _, child := range node.Nodes {
			walkTemplateImports(child, imports)
		}
	case *parse.ActionNode:
		walkTemplateImportPipe(node.Pipe, imports)
	case *parse.IfNode:
		walkTemplateImportPipe(node.Pipe, imports)
		walkTemplateImports(node.List, imports)
		walkTemplateImports(node.ElseList, imports)
	case *parse.RangeNode:
		walkTemplateImportPipe(node.Pipe, imports)
		walkTemplateImports(node.List, imports)
		walkTemplateImports(node.ElseList, imports)
	case *parse.WithNode:
		walkTemplateImportPipe(node.Pipe, imports)
		walkTemplateImports(node.List, imports)
		walkTemplateImports(node.ElseList, imports)
	case *parse.TemplateNode:
		walkTemplateImportPipe(node.Pipe, imports)
	}
}

func walkTemplateImportPipe(pipe *parse.PipeNode, imports map[string]struct{}) {
	if pipe == nil {
		return
	}
	for _, command := range pipe.Cmds {
		if len(command.Args) < 2 {
			continue
		}
		identifier, ok := command.Args[0].(*parse.IdentifierNode)
		if !ok || identifier.Ident != "import" {
			continue
		}
		filename, ok := command.Args[1].(*parse.StringNode)
		if ok {
			imports[filename.Text] = struct{}{}
		}
	}
}

func (c *TemplateContext) renderManagedTemplate(name string, trees map[string]*parse.Tree) (string, error) {
	if err := c.requestContextErr(); err != nil {
		return "", err
	}
	state := c.ensureRenderState()
	c.NewTemplate(name)
	if err := addParsedTemplateTrees(c.tpl, trees, name, name); err != nil {
		return "", err
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	writer := &expansionLimitedWriter{
		buf:               buf,
		state:             state,
		limit:             c.policy().MaxExpansionBytes,
		childBytesAtStart: state.currentChildBytes(),
	}
	if err := c.tpl.Execute(writer, c); err != nil {
		return "", err
	}
	if err := c.requestContextErr(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (c *TemplateContext) executeManagedTemplateInBuffer(tplName string, buf *bytes.Buffer) error {
	state := c.ensureRenderState()
	if err := c.requestContextErr(); err != nil {
		return err
	}
	shouldPromote := !state.hasActiveNodes()
	popRoot := func() {}
	if shouldPromote {
		popRoot = state.pushRoot("response:" + tplName)
	}
	defer popRoot()

	trees, err := c.parseManagedTemplate(tplName, buf.String())
	if err != nil {
		return err
	}
	output, err := c.renderManagedTemplate(tplName, trees)
	if err != nil {
		return err
	}
	if shouldPromote {
		state.promotePendingTemplates(c.config.templateCache, time.Now())
	}
	buf.Reset()
	_, err = buf.WriteString(output)
	return err
}

func (c *TemplateContext) includeManaged(filename string, args ...any) (string, error) {
	state := c.ensureRenderState()
	if err := c.requestContextErr(); err != nil {
		return "", err
	}
	rootKey, filePath, err := c.managedTemplateLocation(filename)
	if err != nil {
		return "", err
	}
	leave, err := state.enter("file:"+rootKey+":"+filePath, c.policy().MaxIncludeDepth)
	if err != nil {
		return "", err
	}
	defer leave()

	trees, err := c.managedTemplateTrees(rootKey, filePath)
	if err != nil {
		return "", err
	}
	c.Args = args
	childBytesBefore := state.currentChildBytes()
	output, err := c.renderManagedTemplate(filePath, trees)
	if err != nil {
		return "", err
	}
	childOutput := len(output) - int(state.currentChildBytes()-childBytesBefore)
	if childOutput > 0 {
		state.addChildOutput(childOutput)
	}
	return output, nil
}

func (c *TemplateContext) importManaged(filename string) (string, error) {
	state := c.ensureRenderState()
	if err := c.requestContextErr(); err != nil {
		return "", err
	}
	rootKey, filePath, err := c.managedTemplateLocation(filename)
	if err != nil {
		return "", err
	}
	nodeKey := "file:" + rootKey + ":" + filePath
	trees, err := c.managedTemplateTrees(rootKey, filePath)
	if err != nil {
		return "", err
	}
	rootDepth := state.currentDepth() + 1
	if rootDepth > c.policy().MaxIncludeDepth {
		return "", errMaxIncludeDepth
	}
	if validatedDepth, ok := state.importValidationDepth(nodeKey); !ok || rootDepth > validatedDepth {
		if err := c.validateImportGraph(rootKey, trees, []string{nodeKey}); err != nil {
			return "", err
		}
		state.markImportValidated(nodeKey, rootDepth)
	}
	if c.tpl == nil {
		c.NewTemplate(filePath)
	}
	return "", addParsedTemplateTrees(c.tpl, trees, filePath, c.tpl.Name())
}

func (c *TemplateContext) managedTemplateTrees(rootKey, filePath string) (map[string]*parse.Tree, error) {
	leftDelim, rightDelim := c.templateDelimiters()
	version := templateCacheVersionKey{
		root:        rootKey,
		path:        filePath,
		delimiters:  leftDelim + "\x00" + rightDelim,
		funcVersion: c.config.funcVersion,
	}
	state := c.ensureRenderState()
	if c.config.templateCache != nil {
		if entry, ok := c.config.templateCache.peek(version, time.Now()); ok {
			return cloneTemplateTrees(entry.trees), nil
		}
	}
	if entry, ok := state.pendingTemplateVersion(version); ok {
		return cloneTemplateTrees(entry.trees), nil
	}
	file, info, err := c.openManagedTemplateFile(rootKey, filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	key := c.cacheKey(rootKey, filePath, info)
	if c.config.templateCache != nil {
		if entry, ok := c.config.templateCache.get(key, info, time.Now()); ok {
			return cloneTemplateTrees(entry.trees), nil
		}
	}
	content, err := c.readManagedTemplateFile(file, filePath)
	if err != nil {
		return nil, err
	}
	trees, err := c.parseManagedTemplate(filePath, content)
	if err != nil {
		return nil, err
	}
	state.storePendingTemplate(key, pendingTemplateEntry{
		rootName: filePath,
		trees:    cloneTemplateTrees(trees),
		info:     info,
	})
	return trees, nil
}

func (c *TemplateContext) readFileManaged(filename string) (string, error) {
	rootKey, filePath, err := c.managedTemplateLocation(filename)
	if err != nil {
		return "", err
	}
	file, err := c.loadManagedTemplateFile(rootKey, filePath)
	if err != nil {
		return "", err
	}
	return file.content, nil
}

func (c *TemplateContext) httpIncludeManaged(uri string) (string, error) {
	state := c.ensureRenderState()
	if err := c.requestContextErr(); err != nil {
		return "", err
	}

	reqCtx := context.WithValue(c.Req.Context(), renderStateContextKey, state)
	virtReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, uri, nil)
	if err != nil {
		return "", err
	}
	leave, err := state.enter("http:"+virtReq.URL.String(), c.policy().MaxIncludeDepth)
	if err != nil {
		return "", err
	}
	defer leave()

	virtReq.Host = c.Req.Host
	virtReq.RemoteAddr = "127.0.0.1:10000"
	virtReq.Header = c.Req.Header.Clone()
	virtReq.Header.Set("Accept-Encoding", "identity")
	virtReq.Trailer = c.Req.Trailer.Clone()
	virtReq.Header.Set(recursionPreventionHeader, strconv.Itoa(state.currentDepth()))

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	vrw := &virtualResponseWriter{body: buf, header: make(http.Header)}
	server := c.Req.Context().Value(caddyhttp.ServerCtxKey).(http.Handler)
	startExpanded := state.expandedBytes()
	childBytesBefore := state.currentChildBytes()
	var responseBytes int64
	vrw.onWrite = func(n int) error {
		if vrw.header.Get(templatesRenderedHeader) == "1" {
			return nil
		}
		limit := c.policy().MaxExpansionBytes
		if limit >= 0 && state.expandedBytes()+responseBytes+int64(n) > limit {
			return errMaxExpansionSize
		}
		responseBytes += int64(n)
		return nil
	}

	server.ServeHTTP(vrw, virtReq)
	if vrw.writeErr != nil {
		return "", vrw.writeErr
	}
	if err := c.requestContextErr(); err != nil {
		return "", err
	}
	if vrw.status >= http.StatusBadRequest {
		return "", fmt.Errorf("http %d", vrw.status)
	}

	output := buf.String()
	if vrw.header.Get(templatesRenderedHeader) != "1" {
		state.setExpanded(startExpanded)
		trees, err := c.parseManagedTemplate(uri, buf.String())
		if err != nil {
			return "", err
		}
		output, err = c.renderManagedTemplate(uri, trees)
		if err != nil {
			return "", err
		}
	}
	childOutput := len(output) - int(state.currentChildBytes()-childBytesBefore)
	if childOutput > 0 {
		state.addChildOutput(childOutput)
	}
	return output, nil
}

func (c TemplateContext) funcPlaceholder(name string) (string, error) {
	repl := c.Req.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// For safety, we don't want to allow the file placeholder in
	// templates because it could be used to read arbitrary files
	// if the template contents were not trusted.
	repl = repl.WithoutFile()

	value, _ := repl.Get(name)

	// propagate the request-body limit marker (a 413 from the request_body
	// max_size limit when {http.request.body} is truncated) so the template
	// aborts with that status rather than rendering a silently-truncated
	// body; the marker is wrapped in a status-carrying handler error so the
	// server renders the 413, and any other value, including unrelated
	// errors, is converted to a string as before
	if err, ok := value.(error); ok {
		if bodyLimit, ok := errors.AsType[caddyhttp.RequestBodyLimitError](err); ok {
			return "", caddyhttp.HandlerError{Err: bodyLimit, StatusCode: bodyLimit.StatusCode()}
		}
	}
	return caddy.ToString(value), nil
}

func (TemplateContext) funcEnv(varName string) string {
	return os.Getenv(varName)
}

// Cookie gets the value of a cookie with name.
func (c TemplateContext) Cookie(name string) string {
	cookies := c.Req.Cookies()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

// RemoteIP gets the IP address of the connection's remote IP.
func (c TemplateContext) RemoteIP() string {
	ip, _, err := net.SplitHostPort(c.Req.RemoteAddr)
	if err != nil {
		return c.Req.RemoteAddr
	}
	return ip
}

// ClientIP gets the IP address of the real client making the request
// if the request is trusted (see trusted_proxies), otherwise returns
// the connection's remote IP.
func (c TemplateContext) ClientIP() string {
	address := caddyhttp.GetVar(c.Req.Context(), caddyhttp.ClientIPVarKey).(string)
	clientIP, _, err := net.SplitHostPort(address)
	if err != nil {
		clientIP = address // no port
	}
	return clientIP
}

// Host returns the hostname portion of the Host header
// from the HTTP request.
func (c TemplateContext) Host() (string, error) {
	host, _, err := net.SplitHostPort(c.Req.Host)
	if err != nil {
		if !strings.Contains(c.Req.Host, ":") {
			// common with sites served on the default port 80
			return c.Req.Host, nil
		}
		return "", err
	}
	return host, nil
}

// funcStripHTML returns s without HTML tags. Similar to PHP's strip_tags()
func (TemplateContext) funcStripHTML(s string) string {
	var buf bytes.Buffer
	depth := 0
	var quoteChar rune
	for _, ch := range s {
		switch {
		case depth > 0 && quoteChar == 0 && (ch == '"' || ch == '\''):
			// entering a quoted attribute value
			quoteChar = ch
		case depth > 0 && ch == quoteChar:
			// leaving a quoted attribute value
			quoteChar = 0
		case ch == '<' && quoteChar == 0:
			depth++
		case ch == '>' && quoteChar == 0:
			if depth > 0 {
				depth--
			} else {
				buf.WriteRune(ch) // stray '>' with no opening '<', keep it
			}
		default:
			if depth == 0 {
				buf.WriteRune(ch)
			}
		}
	}
	return buf.String()
}

// funcMarkdown renders the markdown body as HTML. The resulting
// HTML is NOT escaped so that it can be rendered as HTML.
func (TemplateContext) funcMarkdown(input any) (string, error) {
	inputStr := caddy.ToString(input)

	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Footnote,
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(
					chromahtml.WithClasses(true),
				),
			),
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			gmhtml.WithUnsafe(), // TODO: this is not awesome, maybe should be configurable?
		),
	)

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	err := md.Convert([]byte(inputStr), buf)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// funcSplitFrontMatter parses front matter out from the beginning of input,
// and returns the separated key-value pairs and the body/content. input
// must be a "stringy" value.
func (TemplateContext) funcSplitFrontMatter(input any) (parsedMarkdownDoc, error) {
	meta, body, err := extractFrontMatter(caddy.ToString(input))
	if err != nil {
		return parsedMarkdownDoc{}, err
	}
	return parsedMarkdownDoc{Meta: meta, Body: body}, nil
}

// funcListFiles reads and returns a slice of names from the given
// directory relative to the root of c.
func (c TemplateContext) funcListFiles(name string) ([]string, error) {
	if c.Root == nil {
		return nil, fmt.Errorf("root file system not specified")
	}

	dir, err := c.Root.Open(path.Clean(name))
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	stat, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("%v is not a directory", name)
	}

	dirInfo, err := dir.Readdir(0)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(dirInfo))
	for i, fileInfo := range dirInfo {
		names[i] = fileInfo.Name()
	}

	return names, nil
}

// funcFileExists returns true if filename can be opened successfully.
func (c TemplateContext) funcFileExists(filename string) (bool, error) {
	if c.Root == nil {
		return false, fmt.Errorf("root file system not specified")
	}
	file, err := c.Root.Open(filename)
	if err == nil {
		file.Close()
		return true, nil
	}
	return false, nil
}

// funcFileStat returns Stat of a filename
func (c TemplateContext) funcFileStat(filename string) (fs.FileInfo, error) {
	if c.Root == nil {
		return nil, fmt.Errorf("root file system not specified")
	}

	file, err := c.Root.Open(path.Clean(filename))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return file.Stat()
}

// funcHTTPError returns a structured HTTP handler error. EXPERIMENTAL; SUBJECT TO CHANGE.
// Example usage: `{{if not (fileExists $includeFile)}}{{httpError 404}}{{end}}`
func (c TemplateContext) funcHTTPError(statusCode int) (bool, error) {
	// Delete some headers that may have been set by the underlying
	// handler (such as file_server) which may break the error response.
	c.RespHeader.Header.Del("Content-Length")
	c.RespHeader.Header.Del("Content-Type")
	c.RespHeader.Header.Del("Etag")
	c.RespHeader.Header.Del("Last-Modified")
	c.RespHeader.Header.Del("Accept-Ranges")

	return false, caddyhttp.Error(statusCode, nil)
}

// funcHumanize transforms size and time inputs to a human readable format.
//
// Size inputs are expected to be integers, and are formatted as a
// byte size, such as "83 MB".
//
// Time inputs are parsed using the given layout (default layout is RFC1123Z)
// and are formatted as a relative time, such as "2 weeks ago".
// See https://pkg.go.dev/time#pkg-constants for time layout docs.
func (c TemplateContext) funcHumanize(formatType, data string) (string, error) {
	// The format type can optionally be followed
	// by a colon to provide arguments for the format
	parts := strings.Split(formatType, ":")

	switch parts[0] {
	case "size":
		dataint, dataerr := strconv.ParseUint(data, 10, 64)
		if dataerr != nil {
			return "", fmt.Errorf("humanize: size cannot be parsed: %s", dataerr.Error())
		}
		return humanize.Bytes(dataint), nil

	case "time":
		timelayout := time.RFC1123Z
		if len(parts) > 1 {
			timelayout = parts[1]
		}

		dataint, dataerr := time.Parse(timelayout, data)
		if dataerr != nil {
			return "", fmt.Errorf("humanize: time cannot be parsed: %s", dataerr.Error())
		}
		return humanize.Time(dataint), nil
	}

	return "", fmt.Errorf("no know function was given")
}

// funcMaybe invokes the plugged-in function named functionName if it is plugged in
// (is a module in the 'http.handlers.templates.functions' namespace). If it is not
// available, a log message is emitted.
//
// The first argument is the function name, and the rest of the arguments are
// passed on to the actual function.
//
// This function is useful for executing templates that use components that may be
// considered as optional in some cases (like during local development) where you do
// not want to require everyone to have a custom Caddy build to be able to execute
// your template.
//
// NOTE: This function is EXPERIMENTAL and subject to change or removal.
func (c TemplateContext) funcMaybe(functionName string, args ...any) (any, error) {
	for _, funcMap := range c.CustomFuncs {
		if fn, ok := funcMap[functionName]; ok {
			val := reflect.ValueOf(fn)
			if val.Kind() != reflect.Func {
				continue
			}
			argVals := make([]reflect.Value, len(args))
			for i, arg := range args {
				argVals[i] = reflect.ValueOf(arg)
			}
			returnVals := val.Call(argVals)
			switch len(returnVals) {
			case 0:
				return "", nil
			case 1:
				return returnVals[0].Interface(), nil
			case 2:
				var err error
				if !returnVals[1].IsNil() {
					err = returnVals[1].Interface().(error)
				}
				return returnVals[0].Interface(), err
			default:
				return nil, fmt.Errorf("maybe %s: invalid number of return values: %d", functionName, len(returnVals))
			}
		}
	}
	c.config.logger.Named("maybe").Warn("template function could not be found; ignoring invocation", zap.String("name", functionName))
	return "", nil
}

// WrappedHeader wraps niladic functions so that they
// can be used in templates. (Template functions must
// return a value.)
type WrappedHeader struct{ http.Header }

// Add adds a header field value, appending val to
// existing values for that field. It returns an
// empty string.
func (h WrappedHeader) Add(field, val string) string {
	h.Header.Add(field, val)
	return ""
}

// Set sets a header field value, overwriting any
// other values for that field. It returns an
// empty string.
func (h WrappedHeader) Set(field, val string) string {
	h.Header.Set(field, val)
	return ""
}

// Del deletes a header field. It returns an empty string.
func (h WrappedHeader) Del(field string) string {
	h.Header.Del(field)
	return ""
}

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// at time of writing, sprig.FuncMap() makes a copy, thus
// involves iterating the whole map, so do it just once
var sprigFuncMap = sprig.TxtFuncMap()

const (
	recursionPreventionHeader = "Caddy-Templates-Include"
	templatesRenderedHeader   = "Caddy-Templates-Rendered"
)
