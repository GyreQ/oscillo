# ai-trace 开发路线图

一个本地 AI Agent 追踪产品：录制 Claude Code / Codex / 各类 AI agent 实际发出的
LLM 请求、流式响应、tool 调用，提供浏览器 Dashboard 回放与分析。

## 总体架构

```
入口 A: base_url 网关（主力，CLI agent 全覆盖）
入口 B: MITM 代理（兜底，桌面端/不可配 agent）   Phase 3
入口 C: 本地日志监听（会话结构 + 历史回放）       Phase 2
              │
              ▼
      统一录制内核（纯透传 + 旁路 SSE 解码）
              ▼
   ~/.ai-trace/ledger.jsonl（元数据账本）
   ~/.ai-trace/bodies/<id>.json（完整请求/响应体）
              ▼
   浏览器 Dashboard（go:embed，零依赖）
```

核心原则（来自 magpie 调研，见 docs/magpie-notes.md）：
- 网关**纯透传**：字节流原样转发，旁路 tee 解码，解析失败不影响请求
- 事件模型借鉴 magpie IR：KStart/KText/KThink/KToolStart/KToolArgs/KStop/KUsage
- 归因三件套：UA 前缀识别 agent、session header + 首条 user message hash 识别会话、
  x-claude-code-agent-id 区分子 agent
- JSONL append-only 账本，记账错误绝不弄断请求
- 零第三方依赖（Phase 1），保证离线可构建、交叉编译无痛

## Phase 1：网关内核 + 双协议解码 + run 命令 + 最小 Dashboard ✅ 已验收

范围：
- [x] IR 事件模型 + SSE 增量分帧器
- [x] Anthropic / OpenAI Chat / OpenAI Responses 三种协议旁路解码器
- [x] 网关：纯透传 + 旁路 tee 解码 + 归因 + JSONL 账本 + body 落盘
- [x] adapter 注册表（JSON 声明式）内置 claude-code / codex / generic
- [x] `ai-trace run` 包装命令（env 注入 + 信号转发）
- [x] `ai-trace serve` / `ai-trace open`（Dashboard）
- [x] 单元测试 + httptest 集成测试（fake 上游端到端）
- [x] Makefile 五平台交叉编译

验收标准：
1. `go test ./...` 全绿 ✅
2. 集成测试：透传字节无损 + 账本完整（model/usage/toolCalls/session/脱敏）✅
3. `make build` 产出 darwin/amd64、darwin/arm64、linux/amd64、linux/arm64、windows/amd64 ✅
4. 手动验收：fake 上游端到端 + Dashboard API + adapters 检测 ✅

交接文档：HANDOFF-phase1.md

## Phase 2：日志采集（入口 C）+ 会话视图 ✅ 已验收

范围：
- [x] Claude Code `~/.claude/projects/**/*.jsonl` 解析（session 树、subagent、tool 结果）
- [x] Codex `~/.codex/sessions/**` 解析（格式待真实样本验证）
- [x] mtime 缓存（未变化文件不重新解析；watch 后台进程暂不需要，页面按需加载）
- [x] Dashboard 会话列表 + 会话回放时间线（消息/思考/工具调用/结果逐条展开）
- [x] serve 内置 collector（无需独立 watch 命令，轮询按需解析）

验收：本机 52 个真实会话文件解析出 28 个有效会话，当前对话可完整回放 ✅

交接文档：HANDOFF-phase2.md

## Phase 3：MITM 入口 B + doctor 诊断 ✅ 已验收

范围：
- [x] HTTPS CONNECT 代理 + 动态证书签发（DNS/IP SAN）
- [x] CA 生成/持久化（~/.ai-trace/ca，私钥 0600）+ 各平台信任引导
- [x] `ai-trace capture on/off`（系统代理开关，三平台实现）
- [x] `ai-trace doctor`（服务探活 / CA 状态 / agent 检测 / 接入建议）
- [x] serve 同进程起 MITM(:8788)，解 TLS 后按 Host 转发并复用录制管线

验收：MITM 端到端测试 + 实抓（curl 走 MITM 打真实 API 401 预期且账本完整）、
capture 系统代理开关实测、doctor 输出正确 ✅

已知限制：capture off 不还原原代理（见 HANDOFF-phase3.md，下一版修）。

交接文档：HANDOFF-phase3.md

## Phase 4：分析统计 + 导出 ✅ 已验收

范围：
- [x] token/成本统计（内置价格表 + ~/.ai-trace/prices.json 覆盖，前缀匹配）
- [x] 按模型/agent/日/工具多维聚合（/api/stats）
- [x] Dashboard「统计」标签页（汇总卡片 + 按日条形 + 分组表）
- [x] `ai-trace export`（JSON 报告：记录 + 统计）

验收：真实录制数据聚合正确、导出正常、五平台编译 ✅

交接文档：HANDOFF-phase4.md

---

**四个 Phase 全部完成（2026-10-17）。** 后续迭代建议见 HANDOFF-phase4.md 末节。

## Phase 5：交互与展示重做（对标 magpie）✅ 已验收

用户验收结论：UI/交互/展示与 magpie 差距大。调研确认 magpie 的"客户端"是
手写 HTML/CSS/JS + Wails 壳，因此路线：**Web UI 彻底重做到位，壳后补**。

范围：
- [x] 实时事件总线 internal/live（req_start / event / req_end）+ 网关接线
- [x] `GET /api/live` SSE 推送：正在录制的请求 token 逐字出现在页面（LIVE 面板）
- [x] API 增强：records 支持 session/model/status 过滤；stats 支持 days 参数
- [x] 前端整体重做（static/ 目录化）：顶栏导航、概览 / 请求 / 会话 / 统计四视图
- [x] 请求回放器：对话流（user/assistant/thinking 折叠/tool 卡片）+ 元数据侧栏
  （耗时、TTFT、token 构成条），告别"展开看 JSON"
- [x] 会话上下文 diff：同 session 相邻请求 messages 对比（LCS 对齐），看上下文怎么膨胀
- [x] 统计升级：时间范围切换（今天/7天/30天/全部）+ 大数字卡片 + hover 明细
- [x] 界面文案：每行带说明文字（学 magpie：界面自己会教用户）

验收：构建全绿 ✅ + 真实请求 LIVE 观察成功（46 个 token 级事件逐字推送）✅ +
五平台编译 ✅。交接文档：HANDOFF-phase5.md。

后续（非本期）：Wails 壳复用同一前端、capture off 还原原代理。

## Phase 6：桌面客户端（安装版，对标 magpie 形态）✅ 已实测

用户目标：安装版客户端补齐功能 + UI 完全对标 magpie（Wails 壳复用 web 前端）。

范围：
- [x] `desktop/` 独立 Go 模块（Wails v3 beta.24；主模块保持零依赖）
  - 启动即后端：8787 已有 serve 在跑则复用，否则进程内起网关 + MITM + Dashboard
  - 主窗口加载 `http://127.0.0.1:8787/?desktop=1`（SSE 走真实 HTTP，规避
    WKURLSchemeHandler 不支持流式的问题）；Mac 标题栏藏进页面（红绿灯悬浮）
  - 菜单栏托盘：打开窗口 / Capture 开关（标签随状态）/ 数据目录 / 浏览器 / 退出
  - 关窗 = 隐藏（托盘常驻继续录制）；macOS 不启用 SingleInstance（会闪退，magpie 同）
- [x] `internal/capture` 包：系统代理 MITM 开关抽公共逻辑，CLI / Web API / 托盘三方共用
- [x] Web API：`GET/POST /api/capture`（UI 按钮 + 桌面壳共用）
- [x] 前端推倒重做（magpie 设计语言）：token 级对齐 magpie 实测值
  （浅色 #f4f4f6/#fff/#4f46e5 + 暗色镜像 + 13px 系统字 + tabular-nums）
  - 顶栏：品牌 / 居中分段导航（thumb 滑动）/ REC 呼吸灯 / Capture pill / 主题切换
  - KPI 四宫格（1px 分隔线语法）、白卡列表 + 发丝分隔行、accent 双段柱状图、
    chips 分段选择、pop 样式 tooltip
  - LIVE 逐字流（accent 光标）、回放器 magpie 化、diff 软色块、底部状态栏
  - 深浅主题：跟随系统 + 手动切换（localStorage，加载前应用防闪烁）
- [x] 桌面打包：`desktop/Makefile`（go build + 手动组装 .app + icns）

验收：desktop 编译 ✅、.app 启动 + 窗口 SSE 长连接 ✅、复用已跑 serve 不重复起后端 ✅、
真实流式请求（glm-5.3）落账本 ✅、Capture on→8788 / off→还原 7897 ✅、
DOM 结构断言 + JS 语法全过 ✅。视觉人工验收待用户（本机 headless Chrome 怪癖依旧）。
交接文档：HANDOFF-phase6.md。
