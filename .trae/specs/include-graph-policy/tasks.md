# Templates Include Graph Policy - 实施计划

## Task 1: 增加 policy 配置、生命周期和 Caddyfile 解析
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: None
- **Description**:
  - 在 `Templates` 中增加可选 `IncludeGraph` 配置和编译缓存持有字段。
  - 实现 JSON 字段、默认值、校验、函数指纹生成、Provision 初始化与 Cleanup 关闭。
  - 在 Caddyfile `templates` 块中解析 `include_graph` 嵌套块及全部字段。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-8, AC-10
- **Test Requirements**:
  - `rule` TR-1.1: 完整 Caddyfile 输入解析出等价 policy 字段，非法 bool、size、duration、depth、eviction 返回错误。
  - `rule` TR-1.2: 未配置 policy 时 `IncludeGraph == nil` 且不初始化缓存。
  - `rule` TR-1.3: Provision 生成默认值和函数指纹；Cleanup 后缓存关闭并清空。
  - `rubric` TR-1.4: 配置命名和注释符合 Caddy/Go 风格；scale 1-5；anchors 1/3/5；threshold >= 4；证据为代码审查。
- **Completion Evidence**:
  - `TestIncludeGraphPolicyCaddyfile`、`TestIncludeGraphPolicyJSONAndDefaults` 覆盖完整 Caddyfile/JSON、默认值和非法配置。
  - `TestIncludeGraphCleanupIsolatesInstances` 验证 Cleanup 清空且关闭旧缓存。
  - `go test -race ./modules/caddyhttp/templates` 与 `go vet ./modules/caddyhttp/templates` 通过。
  - Rubric 4/5：类型、JSON/Caddyfile 命名和生命周期遵循现有模块风格；最终分数留待独立审查。

## Task 2: 实现隔离、失效、容量和生命周期的编译模板缓存
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: Task 1
- **Description**:
  - 新增缓存 key、文件身份、parse tree entry、TTL、LRU/random 淘汰和并发控制。
  - 通过 `os.SameFile` 与文件元数据验证变化，按文件检查间隔跳过 stat。
  - 使用 `text/template/parse` 缓存可复制的编译结果，避免共享 per-request template 被污染。
- **Acceptance Criteria Addressed**: AC-3, AC-4, AC-8, AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-2.1: 同身份连续渲染命中，root/path/delimiter/函数指纹/文件身份变化不命中。
  - `rule` TR-2.2: 修改文件和替换文件均使旧 entry 失效，TTL 到期不命中。
  - `rule` TR-2.3: 达到容量时按 LRU 或 random 策略淘汰；关闭后 get/put 不访问旧 map。
  - `rule` TR-2.4: 并发命中、填充和失效在 `-race` 下无竞争。
- **Completion Evidence**:
  - `TestIncludeGraphCompiledCacheHitAndIsolation` 验证命中、函数版本和 delimiter key 隔离。
  - `TestIncludeGraphFileChangesInvalidateCache` 覆盖内容/mtime 变化和文件替换；`TestIncludeGraphCacheCapacityTTLAndCheckInterval` 覆盖 LRU、TTL 与检查间隔。
  - `TestIncludeGraphCleanupIsolatesInstances` 验证关闭后的旧缓存不可写入。
  - `go test -race ./modules/caddyhttp/templates` 通过。
  - Rubric 4/5：缓存惰性淘汰、无后台 goroutine、parse tree 按请求复制；最终分数留待独立审查。

## Task 3: 建立请求级 include graph 状态和展开边界
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: Task 2
- **Description**:
  - 为每个顶层请求建立独立 active path、深度和展开字节状态。
  - 在 include、import、httpInclude 之间共享状态，支持活动路径弹出后的 DAG 复用。
  - 限制单文件读取、HTTP 子响应大小和累计渲染输出；传播请求取消。
- **Acceptance Criteria Addressed**: AC-5, AC-6, AC-7, AC-9
- **Test Requirements**:
  - `rule` TR-3.1: 同文件兄弟分支重复引用成功，A→B→A、递归 import 和 httpInclude 活动 URI 返回 cycle error。
  - `rule` TR-3.2: 超过深度、单文件大小或展开字节数返回错误且目标 buffer 无部分输出。
  - `rule` TR-3.3: 已取消 context 阻止 httpInclude 并防止缓存写入。
  - `rule` TR-3.4: 多个顶层请求的 active path 和预算互不影响。
- **Completion Evidence**:
  - `TestIncludeGraphReuseAndCycles` 覆盖 DAG 合法复用、include 循环、import 循环和 httpInclude 循环。
  - `TestIncludeGraphLimitsAndAtomicOutput` 覆盖文件大小、深度和展开字节限制。
  - `TestIncludeGraphHTTPIncludeAndCancellation` 验证正常 httpInclude、循环和 context cancellation。
  - 每个测试通过独立 `TemplateContext`/render state 隔离请求；`go test -race ./modules/caddyhttp/templates` 通过。
  - Rubric 4/5：活动路径、import frame 和预算使用互斥状态保护；最终分数留待独立审查。

## Task 4: 将 include/import/httpInclude 切换到 policy 原子渲染路径
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: Task 3
- **Description**:
  - policy 启用时用临时 buffer 完成 parse 和 execute，成功后才提交到目标 buffer。
  - include 使用缓存 parse tree 创建请求级 template；import 将复制的 tree 合并进当前 template，失败时不产生部分定义。
  - 保持 nil policy 走现有代码路径和固定 httpInclude 三层保护。
- **Acceptance Criteria Addressed**: AC-2, AC-4, AC-5, AC-6, AC-7, AC-10
- **Test Requirements**:
  - `rule` TR-4.1: parse error、execute error、超限和取消后目标输出 buffer 不出现部分内容。
  - `rule` TR-4.2: import 成功时定义可调用；失败时失败 template 被请求丢弃且缓存无 entry。
  - `rule` TR-4.3: include/import/httpInclude 成功输出与未启用 policy 的现有输出一致。
  - `rubric` TR-4.4: 默认路径与 policy 路径清晰分支、无死代码；scale 1-5；anchors 1/3/5；threshold >= 4；证据为代码审查。
- **Completion Evidence**:
  - `TestIncludeGraphParseErrorsDoNotPolluteCache` 验证 parse error 不写缓存且可恢复。
  - `TestIncludeGraphLimitsAndAtomicOutput` 验证超限后目标 buffer 保留原始 source。
  - `TestIncludeGraphReuseAndCycles`、`TestIncludeGraphHTTPIncludeAndCancellation` 验证 include/import/httpInclude 成功输出；nil policy 继续由原有 `tplcontext_test.go` 覆盖。
  - `go test -race ./modules/caddyhttp/templates` 通过。
  - Rubric 4/5：policy 与默认路径明确分支，parse tree 不跨请求共享可变 template；最终分数留待独立审查。

## Issue I-1: 修复嵌套 include 展开字节重复扣减
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: None
- **Discovered By**: Review R1
- **Description**:
  - 父 include 无条件把整棵子树输出加入全局 `childBytes`，深层 include 已计入的输出被重复扣减，祖先模板自身字节可绕过限制。
- **Acceptance Criteria Addressed**: AC-6, AC-9
- **Test Requirements**:
  - `rule` TR-I-1.1: 深层 include 输出加上祖先自身输出超过限额时必须失败，等于限额时成功。
  - `rule` TR-I-1.2: 兄弟 include 与嵌套 include 均不重复计数。
- **Completion Evidence**:
  - include 仅向父级报告子树新增输出，深层子输出不再重复加入全局 childBytes。
  - `TestIncludeGraphExpansionAccounting` 覆盖 20 字节/15 限额失败、20 字节/20 限额成功和兄弟节点边界。
  - `go test -race -count=1 ./modules/caddyhttp/templates` 通过。

## Issue I-2: 修复 httpInclude 原始响应与本地渲染重复计数
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: I-1
- **Discovered By**: Review R1
- **Description**:
  - 虚拟响应写入时预扣字节，外层随后本地渲染再次计数，渲染成功结算前即可能超过限额。
- **Acceptance Criteria Addressed**: AC-6, AC-7
- **Test Requirements**:
  - `rule` TR-I-2.1: 静态 httpInclude 响应大小等于限额时成功，超过时失败。
  - `rule` TR-I-2.2: 已由内层 templates middleware 渲染的响应只计一次。
- **Completion Evidence**:
  - 未由内层渲染的响应在外层渲染前释放原始响应预扣；已由内层渲染的响应通过结算去重。
  - `TestIncludeGraphHTTPExpansionBoundary` 验证 10 字节响应在 10 字节限额下成功。
  - `go test -race -count=1 ./modules/caddyhttp/templates` 通过。

## Issue I-3: 允许 import 合法复用并只拒绝真实 import 环
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: None
- **Discovered By**: Review R1
- **Description**:
  - import frame 永久保留文件，重复 import 和菱形 DAG 被误判为递归循环；重复 define 也不应比标准库更严格。
- **Acceptance Criteria Addressed**: AC-5, AC-10
- **Test Requirements**:
  - `rule` TR-I-3.1: 同一文件重复 import 与菱形 import 可成功复用。
  - `rule` TR-I-3.2: A→B→A 的常量 import 依赖返回 cycle error。
  - `rule` TR-I-3.3: 重复 define 行为与标准库 Parse 语义一致。
- **Completion Evidence**:
  - 改为静态 import 依赖栈检测真实环，并使用请求级 visited 集合允许重复和菱形复用。
  - 移除自定义重复 define 拦截，沿用 `AddParseTree` 的标准替换语义。
  - `TestIncludeGraphReuseAndCycles` 覆盖环；`TestIncludeGraphImportReuseDepthAndDuplicateDefinitions` 覆盖重复、菱形和标准 define 复用。

## Issue I-4: 将 import 依赖边纳入深度限制
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: I-3
- **Discovered By**: Review R1
- **Description**:
  - 当前 import 不增加深度，纯静态 import 链可超过配置深度继续解析。
- **Acceptance Criteria Addressed**: AC-6
- **Test Requirements**:
  - `rule` TR-I-4.1: 超过 `max_include_depth` 的非环 import 链返回深度错误，限额内链成功。
- **Completion Evidence**:
  - 静态 import 递归使用依赖栈长度在进入下一依赖前检查 `MaxIncludeDepth`。
  - `TestIncludeGraphImportReuseDepthAndDuplicateDefinitions` 验证深度 2 拒绝三节点链、深度 3 成功。

## Issue I-5: 支持 Caddyfile file_change_check -1
- **Status**: `completed`
- **Priority**: `medium`
- **Depends On**: None
- **Discovered By**: Review R1
- **Description**:
  - `caddy.ParseDuration("-1")` 不接受无单位哨兵值，Caddyfile 无法配置禁用主动文件检查。
- **Acceptance Criteria Addressed**: AC-1
- **Test Requirements**:
  - `rule` TR-I-5.1: Caddyfile `file_change_check -1` 解析为 `caddy.Duration(-1)`，正常 duration 仍可解析。
  - `rule` TR-I-5.2: size 解析拒绝超过 int64 上限的值。
- **Completion Evidence**:
  - Caddyfile `file_change_check -1` 特判为 `caddy.Duration(-1)`。
  - `parseCaddyfileSize` 对超过 int64 的值返回 `strconv.ErrRange`。
  - `TestIncludeGraphPolicyCaddyfileSentinelsAndInvalidValues` 覆盖哨兵和非法配置。

## Issue I-6: parse tree 成功渲染后再提升到共享缓存
- **Status**: `completed`
- **Priority**: `medium`
- **Depends On**: I-1, I-2
- **Discovered By**: Review R1
- **Description**:
  - 当前 parse 成功即写共享缓存；执行错误或取消可能把本次请求未成功使用的 parse tree 提前发布。
- **Acceptance Criteria Addressed**: AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-I-6.1: 执行错误或取消后共享缓存无该 entry，同请求内重复引用仍可使用请求级解析结果。
  - `rule` TR-I-6.2: 顶层渲染成功后请求级 entries 原子提升到共享缓存并在下一请求命中。
- **Completion Evidence**:
  - parse tree 存入请求级 pending map，顶层 render 成功后统一 promote；失败/取消随状态丢弃。
  - `TestIncludeGraphExecutionErrorsDoNotPopulateCache` 和取消测试断言共享缓存为 0；既有缓存命中测试验证成功后的下一请求命中。
  - `TestIncludeGraphCacheRootIsolationAndConcurrency` 在 50 个请求并发填充/提升下通过 race。

## Issue I-7: 修复内层模板渲染 httpInclude 的重复计数
- **Status**: `completed`
- **Priority**: `high`
- **Depends On**: I-2
- **Discovered By**: Review R2
- **Description**:
  - 内层 templates middleware 渲染时已通过共享 state 计数，写回 virtualResponseWriter 时又预扣一次，超限可能静默吞掉响应。
- **Acceptance Criteria Addressed**: AC-6, AC-7, AC-9
- **Test Requirements**:
  - `rule` TR-I-7.1: 内层 templates 已渲染的响应在等于限额时成功且不返回空串。
  - `rule` TR-I-7.2: 虚拟响应写入超限时返回错误，不重置或吞掉共享 state 计数。
- **Completion Evidence**:
  - rendered 响应写回不再 claim 共享 state；未渲染响应使用临时 responseBytes 账本。
  - virtualResponseWriter 记录 writeErr，outer httpInclude 在 ServeHTTP 后返回写入错误。
  - `TestIncludeGraphRenderedHTTPIncludeBoundary` 覆盖等于限额成功和未渲染超限失败。

## Issue I-8: import memo 记录校验深度
- **Status**: `completed`
- **Priority**: `medium`
- **Depends On**: I-3
- **Discovered By**: Review R2
- **Description**:
  - 请求级 validatedImports 只记录节点已校验，不记录到达深度，深路径可复用浅路径结果绕过深度。
- **Acceptance Criteria Addressed**: AC-5, AC-6
- **Test Requirements**:
  - `rule` TR-I-8.1: 同一子树先在浅路径通过、后在更深路径引用时，深路径仍返回深度错误。
  - `rule` TR-I-8.2: 相同或更浅路径继续允许合法复用。
- **Completion Evidence**:
  - `validatedImports` 改为记录最小校验深度；更深路径重新校验，相同/更浅路径复用。
  - `TestIncludeGraphImportDepthMemoAndIncludeEdges` 覆盖浅路径校验后深路径仍失败。

## Issue I-9: import 与 include/httpInclude 共享深度预算
- **Status**: `completed`
- **Priority**: `medium`
- **Depends On**: I-8
- **Discovered By**: Review R2
- **Description**:
  - 静态 import DFS 仅计算 import 栈，未计入当前活动 include/httpInclude 边。
- **Acceptance Criteria Addressed**: AC-6
- **Test Requirements**:
  - `rule` TR-I-9.1: include 内连续 import 使总边数超过 max_include_depth 时返回深度错误。
  - `rule` TR-I-9.2: 总边数等于限额时成功。
- **Completion Evidence**:
  - import 根和依赖的目标深度均加入 `state.currentDepth()` 统一计算。
  - `TestIncludeGraphImportDepthMemoAndIncludeEdges` 验证 include+import 超过 2 层失败、3 层成功。

## Task 5: 回归验证、构建和独立审查
- **Status**: `pending`
- **Priority**: `high`
- **Depends On**: Task 4
- **Description**:
  - 运行模板包 race 测试、全仓 short 测试和全仓构建。
  - 修复失败项，执行一次只读独立审查并根据可行动问题回归。
- **Acceptance Criteria Addressed**: AC-1 至 AC-10
- **Test Requirements**:
  - `rule` TR-5.1: `go test -race ./modules/caddyhttp/templates` 通过。
  - `rule` TR-5.2: `go test -short ./...` 通过或记录与本次变更无关的既有环境阻塞。
  - `rule` TR-5.3: `go build ./...` 通过。
  - `rubric` TR-5.4: 独立审查确认所有 AC 有证据且无未解决可行动问题；scale 1-5；anchors 1/3/5；threshold >= 4；证据记录在 review.md。
