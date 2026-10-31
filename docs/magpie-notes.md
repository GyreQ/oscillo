# magpie 架构调研笔记（借鉴来源）

仓库：https://github.com/yetone/magpie （本地克隆于 /Users/asher/Code/magpie）

## 它是什么

本地 LLM 网关（127.0.0.1:3425），定位"任意 Agent 用任意模型"：模型路由、账号池、
额度管理。支持 70+ Agent（含 ZCode、WorkBuddy——验证了这些产品都支持自定义 base URL）。
Go 单二进制，macOS/Windows/Linux/Docker/Termux，TUI+Web+桌面（Wails v3）多形态。

## 对 ai-trace 直接借鉴的机制

1. **IR 中间表示**（internal/gateway/ir.go）：请求侧 Request{Model,System,Messages,Tools}，
   消息内容为 Part（Kind: Text/Image/ToolCall/ToolResult/Thinking）；流式侧 Event
   （KStart/KText/KThink/KToolStart/KToolArgs/KStop/KUsage/KError…）。
   N 协议只需 2N 个转换器而非 N² 组合。
2. **旁路录制**（internal/gateway/capture.go）：包装 ResponseWriter，对客户端的每次
   Write 同时进捕获缓冲（256KiB 内存截断 + 需要全文时 spool 临时文件）。
3. **归因三件套**：
   - UA 前缀识别 agent（gateway.agentOf）
   - session headers 列表（x-claude-code-session-id / session_id / x-conversation-id /
     x-opencode-session）+ 兜底：hash 请求里第一条 user message 生成稳定会话 id
   - x-claude-code-agent-id / x-claude-code-parent-agent-id 区分 subagent
4. **JSONL append-only 账本**（internal/usage）：一行一请求，O_APPEND，错误全部吞掉
   （"记账绝不能弄断请求"），读侧 offset+mtime 增量缓存。
5. **Usage 提取的坑**：只在值 > 0 时覆盖、取最后一次报告（有的协议开头+结尾各报一次）；
   Anthropic 的 input 不含 cache 读写；prompt = input + cache_read + cache_write 统一口径。
6. **上游强制流式**：translate 一律 stream=true，客户端要非流式就自己聚合——录制路径统一。
   ai-trace 不需要（纯透传），但"解码器只处理流式"这个简化可参考。
7. **UI 三层壳**：go:embed 静态资源 + 迷你 JSON API；Wails 桌面/浏览器/CLI 共享；
   `nogui` build tag 出纯 CLI 构建。

## 要避开的

1. 文件组织：gateway.go 6000+ 行、单 agent 适配器 1500+ 行（长期堆叠产物）。
2. 适配器写成手写 Go：magpie 需要原子改写每个 agent 的配置（键/格式/还原全是特判），
   所以必须是代码。ai-trace 不改配置（env 注入，退出即还原），识别信息用声明式 JSON 即可。
3. 账号池/配额窗口/路由组/订阅签到：占其代码量大半，与追踪无关。
4. vendor 淬火（grok_integral/kimi_schema 等）：只在"要把请求发出去"时需要，
   只读旁路产品可全部跳过。
