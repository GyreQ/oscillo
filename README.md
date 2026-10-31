# Oscillo

本地 AI Agent 追踪工具（命令行为 `ai-trace`）：录制 Claude Code / Codex /
各类 AI agent 实际发出的 LLM 请求、流式响应、tool 调用，浏览器里完整回放。

## 特性

- **零证书、零配置接入**：`ai-trace run claude` 一条命令追踪启动 agent，
  退出即还原，不改任何全局配置
- **纯透传网关**：字节流原样转发（透传无损有测试保证），旁路解码不影响请求
- **多协议**：Anthropic Messages / OpenAI Chat / OpenAI Responses（Codex），
  未识别协议也照录原文
- **完整录制**：请求 messages、tools 定义、SSE 逐事件、tool_use 参数增量、
  token 用量（含缓存读写）、TTFT、错误
- **会话回放**：直接解析 Claude Code / Codex 本地日志，历史会话完整回放
  （含 subagent、thinking、工具结果）
- **MITM 兜底**：不支持自定义 base_url 的客户端走系统代理抓取（动态证书，
  环境变量信任优先，不动系统信任链也可用）
- **成本统计**：内置主流模型价格表（可覆盖），按模型/agent/日/工具聚合
- **会话归因**：自动识别 agent（UA）、会话（session 头 / 内容指纹）、子 agent
- **隐私**：一切数据只在本地 `~/.ai-trace/`，密钥脱敏存储
- **跨平台**：macOS / Windows / Linux 单二进制，零第三方依赖，离线可构建

## 快速开始

```sh
go build -o ai-trace ./cmd/ai-trace   # 或 make build

# 追踪启动 Claude Code（自动拉起网关）
ai-trace run claude

# 追踪 Codex
ai-trace run codex

# 追踪任意支持 base_url 的程序
ai-trace run -- python my_agent.py

# 不支持 base_url 的桌面端：系统代理模式（需信任 CA，有引导）
ai-trace capture on     # 开启系统代理 → MITM 录制
ai-trace capture off    # 关闭

# 诊断本机 agent 与接入建议
ai-trace doctor

# 导出报告
ai-trace export --out report.json

# 打开 Dashboard
ai-trace open        # http://127.0.0.1:8787

# 桌面客户端（安装版）：启动即录制 + 菜单栏托盘 + Capture 开关
cd desktop && make app && open Oscillo.app
```

## 下载与安装

已打包的版本在 [Releases](https://github.com/GyreQ/oscillo/releases) 下载。

## 成品打包

```sh
make product                 # 或 scripts/release.sh 0.1.0
```

产物在 `release/v<版本>/`：

- `Oscillo-macos.dmg` — macOS 通用版（Intel + Apple Silicon），内含 CLI
  （`/Applications/Oscillo.app/Contents/MacOS/ai-trace-cli`）
- `Oscillo-windows-amd64.zip` — Windows x64：`Oscillo.exe`（GUI）+
  `ai-trace.exe`（命令行）

两个包均未签名：macOS 首次打开需在「系统设置 → 隐私与安全性」放行；
Windows SmartScreen 选「更多信息 → 仍要运行」。打包同时刷新 `dist/`
下的 CLI 跨平台二进制（含 Linux）。

## 工作原理

```
ai-trace run claude ──► 本地网关 127.0.0.1:8787 ──► api.anthropic.com
                          │ 纯透传（字节无损，有测试保证）
                          │ 旁路: SSE解码 → 事件流 → 聚合 → 归因
                          ▼
              ~/.ai-trace/traces/*.jsonl   元数据账本
              ~/.ai-trace/bodies/*.json    完整请求/响应体
                          ▼
                 Dashboard（同一端口 / 路径）
```

手动接入（不改配置文件的另一种方式）：

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787   # Claude Code 及 Anthropic 兼容
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1   # Codex 及 OpenAI 兼容
```

## 路线

见 [ROADMAP.md](ROADMAP.md)：Phase 2 日志采集与会话回放 → Phase 3 MITM 兜底
与 doctor 诊断 → Phase 4 成本统计与导出。

架构参考了 [magpie](https://github.com/yetone/magpie) 的成熟设计，见
[docs/magpie-notes.md](docs/magpie-notes.md)。

## License

MIT © 2026 [GyreQ](https://github.com/GyreQ)，见 [LICENSE](LICENSE)。
