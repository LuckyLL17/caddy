# handle 分支回退计划（Branch Fallback）实现计划

## 背景与研究结论

### 现状

- Caddyfile 的 `handle` / `handle_path` 在适配期由 [builtins.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/caddyconfig/httpcaddyfile/builtins.go#L823-L825) 的 `parseHandle` 经 `ParseSegmentAsSubroute` 编成若干 `Route`，再由 [httptype.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/caddyconfig/httpcaddyfile/httptype.go#L1486-L1511) 的 `buildSubroute` 放进同一个 `Route.Group`。运行期 [routes.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/routes.go#L286-L297) 的 `wrapRoute` 保证"首个匹配分支执行，同组其余分支跳过"，**没有任何回退机制**，JSON API 中也不存在 `http.handlers.handle` 模块（handle 只是 Route+group 的适配产物）。
- [responsewriter.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/responsewriter.go) 的 `responseRecorder` 可按 `ShouldBufferFunc` 缓冲响应，但其 `Header()` 与底层 writer **共享同一个 header map**（为 trailer 而设计），无法隔离失败尝试写入的 header；1xx（除 101）直接透传；`FlushError` 缓冲时抑制 flush；`Hijack` 透传并标记 hijacked。
- [subroute.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/subroute.go#L72-L81) 是"主路由出错再跑 errors 路由"的模型，错误经 [server.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/server.go#L716-L788) 的 `ServeHTTP` 进入错误链；`HTTPErrorConfig.WithError` 把错误放入 `ErrorCtxKey` 并设置 `{http.error.*}` placeholder。
- 请求级可变状态：`vars` map（[vars.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-Fallback-T21/modules/caddyhttp/vars.go#L450-L479)）、route group map（[server.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/server.go#L1389-L1394)）、replacer 静态表（[replacer.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/replacer.go#L63-L96) 的 `static map`）、以及 `*http.Request` 本身（Method/URL/Header/Host/RequestURI/Body）。replacer 与 vars 均为**每请求独立实例**，尝试间快照/回滚天然并发安全。
- 请求体重放：服务端入站请求 `GetBody` 为 nil；[reverseproxy.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/reverseproxy/reverseproxy.go#L1668-L1693) 的做法是显式配置 `request_buffers` 后整包缓冲，不配置则不保证可重放。本方案沿用同一哲学。
- [intercept.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-handle-branch-fallback-T21/modules/caddyhttp/intercept/intercept.go) 提供了"缓冲→判定→替换响应"的成熟范式与命名响应匹配器用法，可供参照。
- 生命周期：分支内模块经 `ctx.LoadModule` / `RouteList.Provision` 加载，reload/cleanup 由模块生命周期自动覆盖，无需额外清理；`abort` 以 `panic(http.ErrAbortHandler)` 实现，直接穿透 panic，不需要特殊处理。

## 总体设计

### 新增 JSON 模块 `http.handlers.handle`（显式分支集合 + 可选回退计划）

```json
{
  "handler": "handle",
  "branches": [
    { "match": [ { "path": ["/cache/*"] } ], "routes": [ { "handle": [ {"handler": "..."} ] } ] },
    { "routes": [ ... ] }
  ],
  "fallback": {
    "max_attempts": 3,
    "status_codes": [502, 503, 5],
    "response_matchers": [ { "status_code": [504], "headers": {...} } ],
    "error_status_codes": [502, 503],
    "error_match": [ { "expression": {"expr": "{http.error.message} == '...'"} } ],
    "on_exhausted": "commit",
    "request_body_buffer": 1048576,
    "response_body_buffer": 0
  }
}
```

- `branches`：**候选顺序即数组顺序**（确定性，不做排序/归并）。每个分支含 `match`（复用 `http.matchers`）与 `routes`（普通 RouteList，内部仍可嵌套 handle 分组）。
- `fallback` 缺省（nil）时走"未配置快速路径"，语义与现有 grouped handle 完全一致：首个匹配分支以外部 `next` 为尾执行；全不匹配则调用外部 `next`。
- `max_attempts`：最大尝试次数（含首次）；0 = 匹配到的候选数；< 1 的显式值校验报错。
- 响应触发：`status_codes`（支持 `StatusCodeMatches` 类码，如 `5` = 5xx）与 `response_matchers`（`ResponseMatcher`：status+header，OR 关系）。
- 错误触发：`error_status_codes`（对 `HandlerError.StatusCode` 类码匹配）与 `error_match`（普通请求 MatcherSets，评估前用 `WithError` 临时注入 `{http.error.*}`，支持表达式匹配任意错误内容/类型）。
- `on_exhausted`（最终提交策略）：
  - `"commit"`（默认）：候选耗尽时把最后一次尝试缓冲的响应原样提交；若最后一次只有错误没有响应，则返回该错误。
  - `"error"`：不提交任何缓冲响应，回滚所有请求/placeholder 变更后返回最后一次错误；若无错误（被状态码触发），合成 `HandlerError{StatusCode: 最后状态码}` 交给错误路由。
- `request_body_buffer`：0=不缓冲（默认）；-1=不限；>0=字节上限。
- `response_body_buffer`：0=不限（默认，功能本身显式 opt-in）；>0=缓冲上限，超限立即"溢出提交"（已写前缀直送客户端，后续直写，不再回退）。

### Caddyfile：新指令 `try_handle`（普通 `handle` 输出零变化）

```caddyfile
try_handle [<matcher>] {
    max_attempts 3
    fallback_status 502 503 5xx
    fallback_match @slow
    error_status 502 503
    error_match @retryable_error
    on_exhausted commit
    request_body_buffer 1MB
    response_body_buffer 10MB

    @slow {
        status 504
        header Retry-After *
    }
    @retryable_error expression {http.error.message}.startsWith("dial")

    handle /cache/* {
        reverse_proxy localhost:9001
    }
    handle_path /api/* {
        reverse_proxy localhost:9002
    }
    handle {
        respond "maintenance" 503
    }
}
```

- 普通 `handle`/`handle_path` 的适配输出**保持原样**（grouped routes），仅在使用 `try_handle` 包裹时才生成 `http.handlers.handle` + `fallback`。
- `try_handle` 注册为 handler 指令（自带外层可选 matcher），指令顺序置于 `route` 之后。
- 块内只允许：七个选项子指令、`@name` 匹配器定义、`handle`/`handle_path` 段（作为分支，按出现顺序）。
- `@name` 定义嗅探：段内仅含 `status`/`header` 记号的按**响应匹配器**解析（`ParseNamedResponseMatcher`），其余按请求匹配器解析（`parseMatcherDefinitions`）。
- 分支适配：对每个 handle 段复用已注册的指令函数（得到 `Route{match, handle:[subroute{routes}]}`），再把 subroute 的 `routes` 以 JSON 反序列化方式取出（Raw 片段原样保留），组装为 `HandleBranch{match, routes}`。

### 运行期隔离与提交语义（核心）

每次尝试：

1. **请求快照/恢复**：进入模块时深拷贝 URL（复用 `cloneURL`）、clone Header，记录 Method/Host/RequestURI/RemoteAddr/ContentLength；每次尝试前从快照恢复，请求指针通过 `r.WithContext` 派生。
2. **vars 隔离**：子 context 注入 clone 的 vars map（丢弃失败尝试的 `SetVar`）。
3. **route group 隔离**：子 context 注入全新 group map（同一分支内的 grouped 路由在重试时不会因组已满足而跳过）。
4. **placeholder 隔离**：尝试前 `repl.Snapshot()`，失败尝试后 `repl.Restore(snap)`；最终提交的获胜尝试保留其占位符（供访问日志/后续中间件使用）。
5. **响应隔离**：每次尝试使用新建的 `fallbackResponseWriter`：
   - 持有**独立 header map**（从底层 writer header 基线 clone），尝试间不串扰；
   - 缓冲状态码与 body（`sync.Pool` 复用 buffer），受 `response_body_buffer` 上限约束，超限溢出为直通并立即"提交"、终止回退；
   - `WriteHeader(1xx)`（含 101）：先回放当前 header 再透传并标记已提交，不再回退（1xx 已上连接，无法撤回；101 为终态升级）；
   - `Flush`：缓冲期间抑制延迟到 commit；溢出后直通底层；
   - `Hijack`：直通底层（websocket 等无法缓冲），标记已提交，立即终止回退；
   - trailer：commit 时先回放 `Trailer` 宣告头与普通头 → `WriteHeader` → body → 强制 chunked flush → 回放 `Trailer-Prefix` 实际 trailer，顺序参照 reverse_proxy；
   - 实现 `Unwrap` 以支持 `http.ResponseController`，并实现 `io.ReaderFrom` 透传优化。
6. **请求体重放**：
   - nil body：无事可做；
   - `r.GetBody != nil`：每次尝试调 `GetBody()`；
   - 已整包缓冲（配置允许且未超 `request_body_buffer` 上限）：每次尝试给一个全新 `bytes.Reader`；
   - 未缓冲：body 外套计数 reader；失败尝试**完全未读 body（0 字节）**时仍可尝试下一候选（如处理器在读 body 前就报错）；一旦消费过且不可重放，停止回退，按 `on_exhausted` 处理。
7. **结果分类**（按优先级）：
   - `context.Canceled`（客户端取消）：立即停止并原样返回错误，不提交；
   - hijack/溢出/1xx：已提交，停止回退，返回该尝试的 error/nil；
   - 返回 error：命中错误触发条件 → 丢弃响应并尝试下一候选；否则回滚并把错误向上抛（保持现有错误路由行为）；
   - 匹配但**什么都没写**（status=0，等同 `emptyHandler` 的 unhandled）：视为未接管，继续下一候选；
   - 正常响应：命中响应触发条件 → 回退；否则 commit 缓冲响应并返回 nil。
8. **未匹配任何分支**：调用外部 `next`，与现有 handle 落空语义一致。

### 并发 / reload / cleanup

- 模块配置在 Provision 后只读；所有每请求状态（快照、recorder、buffer）均为局部变量，天然支持并发请求。
- 分支路由与其内部 handler 通过 `RouteList.Provision`/`ctx.LoadModule` 加载，config reload 时由 Caddy 生命周期自动 `Cleanup`（如 reverse_proxy 传输层）。
- buffer 取自/归还 `sync.Pool`，不持有跨请求资源。
- 无 `fallback` 配置时走等价快速路径；Caddyfile 普通 `handle` 适配输出字节级不变（由 adapt 测试保证）。

## 改动文件

- `replacer.go`（根包）：新增 `(*Replacer).Snapshot() map[string]any` 与 `(*Replacer).Restore(map[string]any)`（加锁复制/替换 static 表）。
- `modules/caddyhttp/handle.go`（新增）：`HandleHandler`（`http.handlers.handle`）、`HandleBranch`、`FallbackPlan`；Provision/Validate；无配置快速路径与回退尝试循环；请求快照、body 重放、上下文隔离。
- `modules/caddyhttp/fallback_response_writer.go`（新增）：隔离 header、可缓冲可溢出、支持 1xx/Flush/Hijack/trailer 提交的 recorder。
- `modules/caddyhttp/handle_test.go`（新增）：表驱动单元测试。
- `replacer_test.go`：补充 Snapshot/Restore 用例。
- `caddyconfig/httpcaddyfile/builtins.go`：新增 `parseTryHandle` 并在 `init` 注册 `try_handle`。
- `caddyconfig/httpcaddyfile/directives.go`：默认指令序中在 `route` 后加入 `try_handle`。
- `caddytest/integration/caddyfile_adapt/try_handle.caddyfiletest`（新增）：Caddyfile→JSON 适配快照。
- 无需改动 `modules/caddyhttp/standard/imports.go`（模块位于 caddyhttp 包内，随包注册）。

## 实施步骤（依赖顺序）

1. 根包 `Replacer` 增加 Snapshot/Restore（+小测试），先打通 placeholder 回滚原语。
2. 实现 `fallbackResponseWriter`（header 隔离、缓冲/溢出、1xx、Flush、Hijack、trailer、ReaderFrom、Unwrap）。
3. 实现 `HandleHandler`：类型与 JSON tag、`Provision`（分支 matchers/routes、error_match 模块加载）、`Validate`；无 fallback 快速路径；回退循环（快照/上下文/body 重放/分类/提交/耗尽策略）。
4. 编写 `handle_test.go` 覆盖语义矩阵（见验证）。
5. Caddyfile 适配：`parseTryHandle`（选项解析、两类 @matcher 嗅探、handle 段转分支）、注册与指令序；编写 adapt 快照测试。
6. 全量构建与测试、gofmt/golangci 修复。

## 依赖与注意事项

- 沿用包内既有原语：`StatusCodeMatches`、`ResponseMatcher.Match`、`HTTPErrorConfig.WithError`、`MatcherSets.AnyMatchWithError`、`cloneURL`、`emptyHandler`、`NewResponseRecorder` 的设计约定；不引入新依赖（字节大小解析复用已被 reverse_proxy 使用的 `dustin/go-humanize`）。
- 不导出任何外部依赖类型；新增公开面仅限两个 Replacer 方法与新模块配置类型。
- `error_match` 是通用化的"错误类型"机制：表达式可匹配 `{http.error.status_code}`、`{http.error.message}`、`{http.error.id}` 等，不为错误类型另造枚举。
- `abort` 的 `ErrAbortHandler` panic 不在循环内 recover，保持 stdlib 行为。
- 缓冲有内存开销：响应缓冲默认不限但功能显式 opt-in；请求体缓冲默认关闭，与 reverse_proxy 重试一致；均在文档注释中说明。
- 1xx/Flush/Hijack/全双工流式分支无法回退，为确定性地"不污染响应"，统一策略是已发生不可撤回 I/O 即立即提交终止，并在模块文档注释中写明。

## 验证

- `go build ./...` 与 `go vet`（或 `golangci-lint run`）干净。
- `go test -race -short ./...`（重点 `./...` 根包、`./modules/caddyhttp/...`、`./caddyconfig/...`、`./caddytest/...`）。
- 新增单元测试覆盖：
  1. 无 fallback：首个匹配执行/全不匹配走 next/分支顺序；
  2. 状态码触发回退、类码（5xx）触发、header matcher 触发；
  3. 错误状态码与 error_match（表达式）触发；非触发错误向上抛；
  4. max_attempts 截断；
  5. 响应 header/body 隔离（失败分支不污染最终响应）；
  6. vars 与 placeholder 回滚、请求（URL/Header）恢复；
  7. 请求体：整包缓冲重放、未缓冲且 0 读取可回退、已消费不可重放即终止；
  8. 1xx 提交、Flush 延迟提交、Hijack 提交、trailer 正确回放；
  9. 客户端取消立即停止；
  10. on_exhausted=commit 提交最后响应；on_exhausted=error 返回/合成错误；
  11. 并发请求 `-race`；
  12. 匹配但未写响应（unhandled）跳到下一候选。
- adapt 快照测试：`try_handle.caddyfiletest` 验证 JSON 结构；同时确认既有 handle 相关 adapt 测试零变化（`handle_path*`、`handle_nested_in_route` 等）。
- 参照 AGENTS.md 质量门：表驱动测试、Go idioms、接口守卫、文档注释规范。

## 风险与应对

- **Trailer/header 回放细节错误**：严格对照 reverse_proxy 的 announce→WriteHeader→body→flush→trailer 顺序，并加专门单测。
- **流式/全双工死锁或延迟**：缓冲期间抑制 Flush 是既定取舍；显式文档说明；一旦溢出立即直通。
- **大响应/大 body 内存**：两个缓冲上限可配，请求体默认不缓冲，buffer 池化复用。
- **语义偏差**：无 fallback 时快速路径与适配输出均保持旧行为；分支尾固定为 `emptyHandler`（仅新模块），与普通 handle 非尾穿透的细微差异写入注释。
- **错误路由交互**：触发判断只在模块内进行；未命中触发或 `on_exhausted=error` 时错误照常上抛，由 subroute/server 错误链接管，不改变既有错误处理。
