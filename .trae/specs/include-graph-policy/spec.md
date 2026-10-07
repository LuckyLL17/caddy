# Templates Include Graph Policy - 产品需求文档

## Overview
- **Summary**: 为 `templates` middleware 增加可选的 include graph policy，用于缓存已编译模板、校验文件身份、限制 include/import/httpInclude 展开边界，并在配置 reload 或异常时可靠释放缓存。
- **Purpose**: 降低复杂站点中频繁变化模板的重复解析开销，同时防止递归循环、超大文件和过量展开消耗 CPU/内存。
- **Target Users**: 使用 Go templates、多个 `include`/`import`/`httpInclude` 相互引用文件的 Caddy 用户与运维人员。

## Goals
- 通过 JSON 和 Caddyfile 配置可选 include graph policy。
- 支持编译模板缓存、缓存上限、单文件大小、include 深度、单次渲染展开字节数、文件变化检查间隔、缓存生命周期和淘汰策略。
- 缓存键严格隔离 root、规范化路径、delimiter、函数版本和文件身份。
- 每个请求保持独立的模板上下文与渲染状态；允许 DAG 中的合法复用，拒绝活动路径上的递归循环。
- 在超限、parse error、文件变化、请求取消或配置 reload 时不写入部分输出、不污染缓存、不保留旧版本资源。
- 未配置 policy 时完全保留当前输出、错误、httpInclude 递归限制和资源行为。

## Non-Goals
- 不缓存 `httpInclude` 的上游响应体；上游响应可能动态变化，只限制并渲染本次响应。
- 不改变模板信任模型、不新增沙箱能力，也不阻止受信任模板访问文件或发起虚拟请求。
- 不引入外部依赖或后台清理 goroutine。
- 不修改 Caddyfile 顶层 `import` 指令的配置图逻辑。

## Background & Context
当前实现位于 [templates.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-template-include-graph-T19/modules/caddyhttp/templates/templates.go)、[tplcontext.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-template-include-graph-T19/modules/caddyhttp/templates/tplcontext.go) 和 [caddyfile.go](file:///Users/tog_11/Documents/ChatGPT/Swe/workspaces/caddy-template-include-graph-T19/modules/caddyhttp/templates/caddyfile.go)。每个请求都会重新解析响应体及 include/import 的文件；`include` 目前没有文件递归活动路径检测，`httpInclude` 仅有固定 3 层请求头保护。

## Functional Requirements
- **FR-1: 配置模型**: `Templates` 增加可选 `include_graph` 配置；JSON 字段使用 snake_case，Caddyfile 使用同名嵌套块。
- **FR-2: 可配置策略**: policy 支持 compiled cache 开关、缓存条目上限、单文件字节上限、include 深度、单次渲染展开字节上限、文件变化检查间隔、缓存 TTL 和 `lru`/`random` 淘汰策略。
- **FR-3: 默认值**: policy 存在时采用安全默认值：启用编译缓存、1024 个条目、1 MiB 单文件、32 层深度、10 MiB 展开量、每次访问检查文件、1 小时 TTL、LRU 淘汰；显式配置覆盖默认值。
- **FR-4: 编译缓存**: include/import 的文件在通过语法解析后才缓存 parse tree；parse error、执行错误、取消或检测到变化的文件不得写入缓存。
- **FR-5: 缓存隔离**: 缓存查找必须区分解析后的 root、clean path、左右 delimiter、函数集指纹和当前文件身份；任一不同都不得复用。
- **FR-6: 文件失效**: 默认每次使用都通过已打开文件的 stat 信息验证身份；可配置检查间隔。替换文件、大小/修改时间/mode 变化或底层文件身份不同必须失效。TTL 到期条目必须淘汰。
- **FR-7: 请求级图状态**: 每个顶层请求创建独立渲染状态，value receiver 嵌套调用和虚拟 HTTP 请求共享同一请求的状态指针；请求结束后状态不可复用。
- **FR-8: 合法复用与循环**: 文件从活动路径弹出后可再次 include/import；活动路径中再次遇到同一文件/URI 必须返回明确 cycle error，而不是依赖栈溢出或固定 httpInclude 计数。
- **FR-9: 深度和体量边界**: include、import、httpInclude 进入下一层前检查深度；读取文件和虚拟响应时限制大小；渲染过程按累计展开字节计数并在超限时失败。
- **FR-10: 原子渲染**: policy 启用时所有解析和执行先写入临时 buffer，成功后才替换目标 buffer；失败不得保留部分输出或部分 parse tree。
- **FR-11: 取消传播**: `httpInclude` 使用外层请求 context；取消的请求不得继续发起虚拟请求或写入缓存，失败输出丢弃。
- **FR-12: Reload 清理**: policy 缓存由 provision 后的 middleware 实例持有；旧实例 Cleanup 时关闭并清空缓存，新配置不得读取旧缓存。
- **FR-13: 默认兼容**: `include_graph` 为空或未配置时，现有代码路径、输出文本、错误类型语义和固定 3 层 httpInclude 保护保持不变。

## Non-Functional Requirements
- **NFR-1: 并发安全**: 缓存 map、LRU 链表和渲染计数必须能在 `-race` 下安全运行。
- **NFR-2: 跨平台**: 文件身份比较优先使用标准库 `os.SameFile`，在非 `http.Dir` 文件系统上有元数据回退，不破坏 Linux/macOS/Windows 构建。
- **NFR-3: 可维护性**: 配置解析、缓存、渲染状态和模板执行职责分离；不导出缓存内部类型。
- **NFR-4: 性能**: 缓存命中时不得重新读取或解析模板内容；淘汰为同步惰性操作，不启动后台 goroutine。
- **NFR-5: 测试性**: JSON/Caddyfile 适配、缓存命中/失效、循环、深度、大小、展开量、取消和 reload 清理均有自动化测试证据。

## Constraints
- **Technical**: 遵循 Go 和 Caddy module 生命周期；使用 `caddy.Duration` 表示 JSON duration，Caddyfile 使用 `caddy.ParseDuration`，字节大小使用 `humanize.ParseBytes`。
- **Business**: 不创建 PR、issue 或对外发布变更；仅在当前工作区实现。
- **Dependencies**: 仅使用标准库和 go.mod 已有依赖。

## Assumptions
- Caddyfile 嵌套块采用以下语法：
  ```caddyfile
  templates {
      include_graph {
          compiled_cache true
          cache_capacity 128
          max_file_size 256KiB
          max_include_depth 16
          max_expansion_bytes 4MiB
          file_change_check 1s
          cache_ttl 5m
          eviction_policy lru
      }
  }
  ```
- `cache_capacity`、`max_file_size`、`max_expansion_bytes` 使用 `-1` 表示不限；`file_change_check: -1` 表示跳过主动 stat、仅受 TTL 约束。
- policy 内默认深度 32 是新 opt-in 行为；未配置 policy 时仍保留当前 httpInclude 固定 3 层限制。

## Acceptance Criteria

### AC-1: JSON 与 Caddyfile 完整支持 policy
- **Type**: `rule`
- **Given**: 配置包含全部 include graph policy 字段，
- **When**: 分别通过 JSON unmarshal/provision 和 Caddyfile parser/adapter 加载，
- **Then**: 每个字段都进入等价的 `Templates.IncludeGraph` 值，非法枚举、负数深度或非法 duration/size 返回配置错误。
- **Pass Condition**: 单元测试覆盖完整字段、默认值和至少一个非法配置。
- **Evidence**: `go test ./modules/caddyhttp/templates`。

### AC-2: 未配置 policy 时保持现有行为
- **Type**: `rule`
- **Given**: 没有 `include_graph` 的 templates handler，
- **When**: 渲染现有模板、include、import、httpInclude 和错误用例，
- **Then**: 输出和错误与当前语义一致，不创建模板编译缓存。
- **Pass Condition**: 现有模板包测试全部通过，默认路径不访问 policy cache。
- **Evidence**: `go test -race ./modules/caddyhttp/templates`。

### AC-3: 编译缓存命中且隔离正确
- **Type**: `rule`
- **Given**: policy 启用编译缓存且文件未变化，
- **When**: 两个请求连续渲染同一 root/path/delimiter/函数集文件，
- **Then**: 第二次复用 parse tree；改变 root、path、delimiter、函数指纹或文件身份任一项都产生缓存未命中。
- **Pass Condition**: 测试可观测缓存命中计数并验证不同 key 不共享 entry。
- **Evidence**: templates 包单元测试与 race 测试。

### AC-4: 文件变化可靠失效
- **Type**: `rule`
- **Given**: 文件已有缓存 entry，
- **When**: 修改内容、大小、mtime/mode 或替换为不同文件身份，
- **Then**: 后续请求重新读取并解析新内容；旧 parse tree 不返回、不覆盖新版本结果。
- **Pass Condition**: 修改和替换场景均断言新输出与缓存 entry 更新。
- **Evidence**: templates 包单元测试。

### AC-5: DAG 复用允许、活动路径循环拒绝
- **Type**: `rule`
- **Given**: policy 启用，
- **When**: 同一文件在不同分支被重复引用，渲染成功；A→B→A 或 httpInclude 回到活动 URI，
- **Then**: 合法复用成功，循环返回明确 cycle error，且不发生栈溢出。
- **Pass Condition**: include、import、httpInclude 的复用/循环测试通过。
- **Evidence**: templates 包单元测试。

### AC-6: 深度、文件大小和展开量受限
- **Type**: `rule`
- **Given**: policy 配置有限深度、单文件大小和展开字节数，
- **When**: 模板超过任一限制，
- **Then**: 请求失败且目标响应 buffer 不包含部分模板输出；未超限时正常渲染。
- **Pass Condition**: 每个限制有独立成功/失败用例。
- **Evidence**: `go test -race ./modules/caddyhttp/templates`。

### AC-7: parse error、取消和失败不污染缓存
- **Type**: `rule`
- **Given**: 模板存在 parse error、请求在 httpInclude 前取消或渲染中超限，
- **When**: 发起后续合法请求，
- **Then**: 缓存中不存在失败 entry，后续合法文件可正常解析和缓存。
- **Pass Condition**: 缓存计数和后续渲染均验证无污染。
- **Evidence**: templates 包单元测试。

### AC-8: Reload 释放旧缓存
- **Type**: `rule`
- **Given**: provision 后的 Templates 实例拥有缓存，
- **When**: 调用 Cleanup 并使用新 provision 的实例，
- **Then**: 旧缓存关闭、清空且不可写入，新实例使用独立缓存。
- **Pass Condition**: Cleanup 后 get/put 不恢复旧 entry，新旧实例 key/entry 不共享。
- **Evidence**: templates 包单元测试。

### AC-9: 并发与资源使用质量
- **Type**: `rubric`
- **Dimension**: 并发安全、无 goroutine/文件句柄泄漏、缓存淘汰符合配置。
- **Scale**: 1-5
- **Anchors**: 1 = race 报告、文件句柄泄漏或淘汰失效；3 = 功能正确但锁粒度过粗或有临时资源风险；5 = 并发命中/失效稳定、文件均关闭、LRU/random/TTL 行为清晰且无后台资源。
- **Pass Threshold**: >= 4
- **Evidence**: race 测试、并发缓存测试和代码审查。

### AC-10: 实现与 Caddy 习惯一致
- **Type**: `rubric`
- **Dimension**: API 命名、配置注释、错误文案、模块生命周期和职责划分。
- **Scale**: 1-5
- **Anchors**: 1 = 新增全局状态或破坏模块模式；3 = 功能可用但配置/类型耦合不清；5 = 类型边界清楚、注释符合 Go/Caddy 风格、默认路径最小改动。
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查与 `go test`、`go build`。
