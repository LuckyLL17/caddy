# Templates Include Graph Policy - Independent Review

- [ ] CP-R1: JSON/Caddyfile policy 配置完整且非法值可校验
  - **Type**: `rule`
  - **Covers**: AC-1
  - **Evidence**: Pending；初审发现 Caddyfile 无法解析承诺的 `file_change_check -1`。

- [ ] CP-R2: 未配置 policy 时保持现有行为
  - **Type**: `rule`
  - **Covers**: AC-2
  - **Evidence**: `go test -race -short ./...` 通过，默认路径保留原实现。

- [ ] CP-R3: 编译缓存按 root、路径、delimiter、函数版本和文件身份隔离
  - **Type**: `rule`
  - **Covers**: AC-3
  - **Evidence**: Pending；基础命中测试通过，但 root/path 隔离与并发测试不足。

- [ ] CP-R4: 文件变化、TTL 和容量淘汰可靠生效
  - **Type**: `rule`
  - **Covers**: AC-4, AC-8, AC-9
  - **Evidence**: 顺序测试通过；初审建议补并发和版本索引。

- [ ] CP-R5: 合法 DAG 复用允许，活动递归循环拒绝
  - **Type**: `rule`
  - **Covers**: AC-5
  - **Evidence**: Fail；重复/菱形 import 被误判为 cycle。

- [ ] CP-R6: 深度、文件大小和展开字节限制准确
  - **Type**: `rule`
  - **Covers**: AC-6
  - **Evidence**: Fail；嵌套 include 少计、httpInclude 峰值重复计数、import 链未占深度。

- [ ] CP-R7: parse/execute/cancel 失败不产生部分输出或共享缓存污染
  - **Type**: `rule`
  - **Covers**: AC-7
  - **Evidence**: Fail；parse tree 在执行完成前进入共享缓存。

- [ ] CP-U1: 并发、资源和 Caddy 模块生命周期质量
  - **Type**: `rubric`
  - **Covers**: AC-9, AC-10
  - **Scale**: 1-5
  - **Anchors**: 1 = race/泄漏/旧缓存污染；3 = 功能可用但状态或资源计数不可靠；5 = 并发稳定、无后台资源、生命周期和默认兼容清晰。
  - **Pass Threshold**: >= 4
  - **Evidence**: Pending；当前 3/5。

## Review History

### Review R1
- **Result**: `fail`
- **Evidence**:
  - `go build ./...` 通过。
  - `go test -race -count=1 ./modules/caddyhttp/templates/` 通过。
  - `go test -short ./...` 通过。
  - `go vet ./modules/caddyhttp/templates/` 通过。
  - `golangci-lint` 因本机未安装而 blocked，不影响静态编译判断。
- **Findings**:
  - **I-1**: `actionable`; high; 嵌套 include 重复累计 `childBytes`，深层模板可导致 `max_expansion_bytes` 少计并绕过限制。
  - **I-2**: `actionable`; high; httpInclude 原始响应和本地渲染瞬时重复计数，等于限额的响应被误杀。
  - **I-3**: `actionable`; high; import frame 永久保留文件，重复 import/菱形 DAG 被误判 cycle。
  - **I-4**: `actionable`; high; import 边不增加深度，纯 import 链可绕过 `max_include_depth`。
  - **I-5**: `actionable`; medium; Caddyfile 的 `file_change_check -1` 被 `caddy.ParseDuration` 拒绝。
  - **I-6**: `actionable`; medium; 执行错误或取消前 parse tree 已进入共享缓存，与 FR-4 不一致。
  - **Advisory**: 补齐非法 Caddyfile、root/path 隔离、成功侧限制、httpInclude 大小、取消后无缓存和并发测试；修复 size 溢出；为缓存版本增加 O(1) 索引；对齐标准库重复 define 替换语义；统一核心函数名单来源。
- **Recommended Issues**:
  - I-1: 修复嵌套 include 展开量计数，high，要求链式/兄弟/转换 include 回归。
  - I-2: 修复 httpInclude 预扣与 render 计数，high，要求边界值成功和超限失败。
  - I-3: 改为静态/活动 import 图检测，允许重复与菱形 DAG，拒绝真实环。
  - I-4: import 图边计入深度，high，要求超过限制的非环链失败。
  - I-5: Caddyfile 特判 `-1` duration，medium。
  - I-6: parse tree 请求级暂存、成功后提升共享缓存，medium，要求执行错误/取消不污染。

### Review R2
- **Result**: `fail`
- **Evidence**:
  - R1 的 I-1、I-3、I-5、I-6 修复验证通过。
  - `go test -race -count=1 ./modules/caddyhttp/templates`、`go build ./...`、`go vet ./modules/caddyhttp/templates` 通过。
  - 复审通过临时只读复现发现内层 templates 渲染的 httpInclude 双重计数和两个 import 深度绕过缺口。
- **Findings**:
  - **I-7**: `actionable`; high; 内层 templates middleware 已计数的响应在写回 vrw 时被临时预扣第二次，超限时写入失败但 outer 可能静默得到空输出；`childBytes` 取样点也偏晚。
  - **I-8**: `actionable`; medium; import visited memo 不记录校验深度，浅路径验证过的子树在深路径复用时跳过检查。
  - **I-9**: `actionable`; medium; 静态 import DFS 未加入当前 include/httpInclude 活动深度，三类边没有共享深度预算。
  - **Advisory**: 静态 walker 会保守拒绝不会执行的条件 import 环；动态 import 只受运行时保护；random 淘汰可补测试。
- **Recommended Issues**:
  - I-7: httpInclude 使用独立临时响应账本，rendered 响应不重复 claim，childBytes 在 ServeHTTP 前取样，并捕获写入错误。
  - I-8: import memo 记录最小到达深度，深度更深时重新校验子图。
  - I-9: import DFS 组合 `state.currentDepth()` 与静态栈长度。
