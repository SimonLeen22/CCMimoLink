# CCMimoLink

> A local routing bridge that brings Xiaomi MiMo to `cc switch` + Codex.

[🌐 Homepage](https://simonleen22.github.io/CCMimoLink/) · [中文文档](#中文) · [🍷 Windows 版本](https://github.com/2144291529/CCMimoLink-win) · [Releases](https://github.com/SimonLeen22/CCMimoLink/releases)

**v2.0** — Anthropic Messages upstream protocol, extended thinking support, tool call hardening, and HTTP resilience improvements.

---

CCMimoLink is not another reverse proxy. It is an active protocol adapter that translates Codex-style requests into a format MiMo can handle reliably — letting you use Xiaomi MiMo models seamlessly within Codex.

```text
User → Codex → CCMimoLink (local proxy) → Xiaomi MiMo upstream
         ↑                                       |
         └── cc switch manages API Key ──────────┘
```

- You manage the Xiaomi MiMo API key in `cc switch` — that's it.
- On startup, CCMimoLink takes over the MiMo route and rewrites it to a local proxy.
- Codex continues sending OpenAI-style `/v1/responses` requests.
- CCMimoLink translates the protocol in-flight before forwarding to MiMo upstream.

## Key Capabilities

### Protocol Adaptation

- **Responses → Chat Completions**: Translates OpenAI-style `/v1/responses` into MiMo's chat-completions format, injecting `instructions` as a system message when needed.
- **Anthropic Messages upstream mode**: Set `MIMO_UPSTREAM_PROTOCOL=anthropic` to route via MiMo's Anthropic Messages API instead of chat/completions. Unlocks extended thinking and higher tool-call fidelity for complex Codex agent tasks.
- **Extended Thinking**: In Anthropic upstream mode, `thinking` blocks are streamed and replayed correctly. Thinking is automatically suppressed when replaying tool history to avoid upstream hangs.
- **Codex Sub-Agent Compatibility**: Flattens Codex multi-agent tool shapes into standard function tools so sub-agent and multi-tool turns remain usable upstream.
- **Tool Compatibility Layer**: Preserves standard function tools, normalizes `tool_choice`, and filters unsupported built-in tools to prevent upstream failures.
- **Multi-turn Continuation**: Maintains bounded in-memory state for response chains, supports `previous_response_id`, and replays function-call context and provider reasoning across turns.
- **Streaming Fidelity**: Preserves true incremental streaming, emitting Responses-style events while reading from the MiMo upstream stream.

### Intelligent Routing

- **Codex-Aware Model Routing**: Keeps text-first and multi-agent turns on `mimo-v2.5-pro`, while preserving Codex-style request semantics.
- **Text-First + Vision Fallback**: Requests containing images automatically route the whole turn to `mimo-v2.5` for compatibility.
- **Codex Model Alias Mapping**: GPT-family request model names from Codex (for example `gpt-5.4` / `gpt-5.4-mini`) are mapped onto MiMo backends automatically so Codex-side model labels do not break upstream routing.
- **Dynamic Model Switching**: Switch between `mimo-v2.5` and `mimo-v2.5-pro` via built-in CLI subcommands (`model set` + `model restart`), environment variables, or startup flags.

### Safety and Resilience

- **HTTP Timeout Protection**: A 60-second `ResponseHeaderTimeout` prevents a non-responding upstream from blocking the connection indefinitely. An idle-stream timeout (60s) closes stalled response bodies automatically, so concurrency slots are never held forever.
- **Concurrency Safety**: Each stream correctly releases its limiter slot on completion or error — no slot leaks under high fan-out.
- **Safe Startup Sync**: Automatically backs up Codex config before rewriting.
- **Throttling and Backoff**: Built-in request rate limiting with upstream `429` retry backoff.
- **XML Fallback Parsing**: Recovers tool-call intent from tool-call-like text output when needed.
- **Local Compact Handling**: Handles `compact` control-plane requests locally instead of letting them fail upstream.

## How It Works

1. User configures the Xiaomi MiMo API key in `cc switch`
2. CCMimoLink starts
3. On startup, CCMimoLink automatically:
   - Verifies `cc switch` is installed
   - Locates the Xiaomi MiMo provider in `cc switch`
   - Rewrites the MiMo route to the local proxy URL
   - Backs up and rewrites the local Codex config
   - Refreshes `X-Mimo-Api-Key` in Codex from `cc switch`
4. Codex requests are routed through the local CCMimoLink proxy
5. CCMimoLink adapts the protocol and forwards to MiMo upstream

## Quick Start

### Step 1: Configure MiMo in cc switch

Add a Xiaomi MiMo provider in `cc switch` and enter your API key.

### Step 2: Build

```bash
go build -o ccmimolink .
```

### Step 3: Sync only (optional)

Rewrite routes and refresh config without starting the proxy:

```bash
./ccmimolink sync
```

### Step 4: Run the proxy

```bash
# Standard mode (OpenAI chat/completions upstream)
./ccmimolink

# Anthropic Messages upstream mode (recommended for complex Codex tasks)
MIMO_UPSTREAM_PROTOCOL=anthropic ./ccmimolink
```

Default local proxy address: `http://127.0.0.1:9876/v1`

### Step 5: Switch models (optional)

Text requests default to `mimo-v2.5-pro`. Use the built-in CLI to switch models and restart the launchd service in one line:

```bash
# Switch to mimo-v2.5 and restart
./ccmimolink model set v2.5 --restart

# Check current status
./ccmimolink model status

# Or use environment variables (non-launchd scenarios)
MIMO_MODEL="mimo-v2.5" ./ccmimolink

# Or use startup flags
./ccmimolink --v2.5
```

> Image-bearing turns always route to `mimo-v2.5`; text-only turns keep using `mimo-v2.5-pro` by default.

## CLI Subcommands

| Command | Description |
| --- | --- |
| `model set <name>` | Set MIMO_MODEL in plist (run `model restart` separately) |
| `model set <name> --restart` | Set model + restart launchd service in one step |
| `model status` | Show current model, process PID, plist info |
| `model restart` | Restart the launchd service (bootout + bootstrap) |
| `sync` | Sync cc switch + Codex config + auth.json (no proxy) |
| `help` | Show full help |

Model aliases: `pro` → `mimo-v2.5-pro`, `v2.5` → `mimo-v2.5`.

## Configuration

All runtime settings are provided via environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `MIMO_API_KEY` | empty | Fallback MiMo upstream API key. Normally the key comes from the `X-Mimo-Api-Key` header; this is only a safety net. |
| `MIMO_BASE_URL` | `https://token-plan-cn.xiaomimimo.com/v1` | MiMo upstream base URL. |
| `MIMO_MODEL` | `mimo-v2.5-pro` | Default text model. |
| `MIMO_PROXY_PORT` | `9876` | Local listen port. |
| `MIMO_UPSTREAM_PROTOCOL` | `openai` | Set to `anthropic` to route via MiMo's Anthropic Messages API instead of chat/completions. Recommended for Codex agent tasks with complex tool use. |
| `MIMO_PROXY_MAX_CONCURRENT` | `4` | Maximum concurrent upstream requests. |
| `MIMO_PROXY_MIN_INTERVAL_MS` | `600` | Minimum delay between upstream requests (ms). |
| `MIMO_PROXY_429_BACKOFF_MS` | `30000` | Backoff duration after an upstream `429` response (ms). |
| `MIMO_PROXY_LOG` | `mimo_proxy.log` | Log file path. |
| `MIMO_PROXY_SKIP_CC_SWITCH_SYNC` | `false` | Skip startup sync (for development). |
| `MIMO_PROXY_LEGACY_MODE` | `false` | Restore conservative pre-v1.2 routing defaults. |
| `MIMO_DEBUG_DUMP` | empty | If set to a directory path, writes upstream request/response pairs for debugging. |
| `CC_SWITCH_SETTINGS_PATH` | `~/.cc-switch/settings.json` | Path to cc switch settings file. |
| `CC_SWITCH_DB_PATH` | `~/.cc-switch/cc-switch.db` | Path to cc switch database. |
| `CODEX_CONFIG_PATH` | `~/.codex/config.toml` | Path to local Codex config. |

## Supported Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/responses` | Primary endpoint — core protocol adaptation entry point. |
| `GET` | `/v1/models` | Model listing. |
| `GET` | `/health` | Health check. |
| `POST` | `/v1/responses/compact` | Returns a local unsupported response. |
| `GET` | `/v1/responses/{id}` | Retrieve a stored response by ID. |

## Compatibility Notes

- Function tool workflows are preserved to the extent MiMo supports them.
- Unsupported built-in tools are filtered rather than crashing the entire request.
- `previous_response_id` continuation is implemented via bounded in-memory storage.
- `parallel_tool_calls` is accepted and forwarded; final behavior depends on MiMo upstream support.
- Image-bearing turns are pinned to `mimo-v2.5`; text-only turns stay on `mimo-v2.5-pro` by default.
- GPT-family Codex request model names are aliased onto MiMo backends automatically.
- In Anthropic upstream mode, consecutive same-role messages are merged into valid alternating format before forwarding.

## Requirements

- Go 1.26.3 or newer
- `cc switch` installed locally
- A configured Xiaomi MiMo API key in `cc switch`
- Local Codex config at `~/.codex/config.toml` (or an overridden path via environment variable)

## Troubleshooting

**`cc switch is not installed or incomplete`**

Verify these files exist:
- `~/.cc-switch/settings.json`
- `~/.cc-switch/cc-switch.db`
- `~/.codex/config.toml`

**`Xiaomi MiMo provider not found`**

Add a Xiaomi MiMo provider in `cc switch` first.

**`Xiaomi MiMo API key is empty`**

Open `cc switch`, edit the Xiaomi MiMo provider, and enter the API key.

**`Current provider is not Xiaomi MiMo`**

This is usually fine. CCMimoLink will automatically locate the Xiaomi MiMo provider and use it for sync.

**`cc switch route still looks incorrect`**

Run `./ccmimolink sync` again, then restart `cc switch`.

**`Codex still uses the old route or key`**

Restart Codex after sync so it reloads `~/.codex/config.toml`.

**`Why do I need to restart cc switch and Codex?`**

Both applications cache provider configuration in memory. Restarting ensures the rewritten route and refreshed `X-Mimo-Api-Key` take effect.

## Changelog

### v2.0
- **Anthropic Messages upstream mode**: new `MIMO_UPSTREAM_PROTOCOL=anthropic` routes via MiMo's Anthropic Messages API, enabling extended thinking and higher tool-call fidelity.
- **Extended thinking support**: thinking blocks are streamed and replayed correctly; automatically suppressed when replaying tool history to prevent upstream hangs.
- **Tool call hardening**: six targeted fixes — guard against empty tool IDs, correct `tool_choice` mapping (`auto`/`required`/`none`/function), multi-system-message concatenation, scanner error recovery, flush guard for incomplete tool calls, and tightened debug file permissions.
- **HTTP timeout protection**: `ResponseHeaderTimeout` (60s) and idle-stream timeout (60s) prevent stalled upstreams from holding concurrency slots.
- **Concurrency slot leak fix**: streams now correctly release limiter slots on all exit paths, eliminating the freeze-under-heavy-fan-out bug.

### v1.2
- Codex sub-agent compatibility improvements, safer multi-agent routing, automatic image fallback to `mimo-v2.5`.

### v1.1
- Built-in CLI subcommands for model management and sync.

## License

MIT License. See [LICENSE](LICENSE).

## Author

[@simonlin](https://x.com/simonlin) · [GitHub](https://github.com/SimonLeen22/CCMimoLink)

---

# 中文

> 面向 `cc switch` + Codex 的 Xiaomi MiMo 本地路由桥接器。

🍷 [Windows 版本（第三方适配）](https://github.com/2144291529/CCMimoLink-win)

CCMimoLink 不是又一个反向代理。它是一个主动协议适配层——把 Codex 风格的请求翻译成 MiMo 能稳定处理的形式，让你在 Codex 里无缝使用 Xiaomi MiMo 模型。

```text
用户 → Codex → CCMimoLink（本地代理） → Xiaomi MiMo 上游
         ↑                                      |
         └──── cc switch 管理 API Key ──────────┘
```

- 你在 `cc switch` 里维护 Xiaomi MiMo 的 API Key，仅此而已
- CCMimoLink 启动时自动接管 MiMo 路由，把请求地址改写到本地代理
- Codex 继续发 OpenAI 风格的 `/v1/responses` 请求
- CCMimoLink 在中间完成协议翻译，再转发给 MiMo 上游

## 核心能力

### 协议适配

- **Responses → Chat Completions**：把 OpenAI 风格的 `/v1/responses` 请求翻译成 MiMo 的 chat-completions 格式，必要时把 `instructions` 注入为 system message
- **Anthropic Messages 上游模式**：通过 `MIMO_UPSTREAM_PROTOCOL=anthropic` 切换到 MiMo 的 Anthropic Messages API，解锁扩展思考（extended thinking）和更高的工具调用保真度，适合复杂 Codex 代理任务
- **扩展思考（Extended Thinking）支持**：Anthropic 上游模式下，`thinking` 块会被正确流式传输和回放；当回放含工具历史的上下文时，自动抑制 thinking 以避免上游卡死
- **Codex 子任务兼容层**：把 Codex 的多子任务工具形态展平成标准 function tool，保证 sub-agent / 多工具轮次在上游可用
- **Tool 兼容层**：保留标准 function tool，规范化 `tool_choice`，过滤不兼容的 built-in tool 避免整条请求崩掉
- **多轮续接**：通过有界内存保存 response-chain 状态，支持 `previous_response_id`，在多轮交互中回放 function-call 上下文和 provider reasoning 状态
- **流式保真**：保留真实的增量流式路径，在读取 MiMo 上游流时发出 Responses 风格事件

### 智能路由

- **面向 Codex 的模型路由**：纯文本和多子任务轮次优先保留在 `mimo-v2.5-pro`，尽量维持 Codex 风格请求语义
- **文本优先 + 视觉回落**：只要请求里带图片，整轮自动切到 `mimo-v2.5` 保证兼容
- **Codex 模型号别名映射**：Codex 侧可能带来的 GPT 风格模型名（例如 `gpt-5.4` / `gpt-5.4-mini`）会自动映射到 MiMo 后端
- **动态模型切换**：通过内置 CLI 子命令（`model set` + `model restart`）、环境变量或启动参数在 `mimo-v2.5` 与 `mimo-v2.5-pro` 之间动态切换

### 安全与韧性

- **HTTP 超时保护**：60 秒 `ResponseHeaderTimeout` 防止无响应的上游无限阻塞连接；60 秒空闲流超时自动关闭停滞的响应体，并发槽不会被永久占用
- **并发安全**：每条流在完成或出错时都正确释放其限流槽，高 fan-out 场景下不会出现槽泄漏
- **启动安全同步**：回写 Codex 配置前自动备份原始文件
- **限流与退避**：自带请求限流和上游 `429` 退避处理
- **XML 兜底解析**：必要时可以从类 tool-call 文本中恢复工具调用意图
- **本地 compact 处理**：对 `compact` 这类控制面请求提供本地处理，不把错误甩给上游

## 工作流程

1. 用户在 `cc switch` 中配置 Xiaomi MiMo 的 API Key
2. 启动 CCMimoLink
3. CCMimoLink 自动完成：
   - 检查 `cc switch` 是否已安装
   - 查找 `cc switch` 中的 Xiaomi MiMo provider
   - 把 MiMo 路由改写为本地代理地址
   - 备份并回写本地 Codex 配置
   - 从 `cc switch` 刷新 Codex 的 `X-Mimo-Api-Key`
4. Codex 请求自动走本地 CCMimoLink
5. CCMimoLink 完成协议适配后转发给 MiMo 上游

## 快速开始

### 第一步：在 cc switch 中配置 MiMo

在 `cc switch` 中添加 Xiaomi MiMo provider 并填入 API Key。

### 第二步：编译

```bash
go build -o ccmimolink .
```

### 第三步：仅同步配置（可选）

只做配置同步，不启动代理服务：

```bash
./ccmimolink sync
```

### 第四步：启动代理

```bash
# 标准模式（OpenAI chat/completions 上游）
./ccmimolink

# Anthropic Messages 上游模式（推荐用于复杂 Codex 代理任务）
MIMO_UPSTREAM_PROTOCOL=anthropic ./ccmimolink
```

默认本地代理地址：`http://127.0.0.1:9876/v1`

### 第五步：切换模型（可选）

纯文本请求默认使用 `mimo-v2.5-pro`。使用内置 CLI 子命令一键切换模型并重启 launchd 服务：

```bash
# 切换到 mimo-v2.5 并重启
./ccmimolink model set v2.5 --restart

# 查看当前状态
./ccmimolink model status

# 也可用环境变量方式（非 launchd 场景）
MIMO_MODEL="mimo-v2.5" ./ccmimolink

# 或使用启动参数
./ccmimolink --v2.5
```

> 带图轮次始终自动切到 `mimo-v2.5`；纯文本轮次默认继续使用 `mimo-v2.5-pro`。

## CLI 子命令

| 命令 | 说明 |
| --- | --- |
| `model set <name>` | 设置 plist 中的 MIMO_MODEL（需再执行 restart） |
| `model set <name> --restart` | 设置模型 + 重启 launchd 服务（一步完成） |
| `model status` | 查看当前模型、进程 PID、plist 信息 |
| `model restart` | 重启 launchd 服务（bootout + bootstrap） |
| `sync` | 同步 cc switch + Codex 配置 + auth.json（不启动代理） |
| `help` | 显示完整帮助 |

模型别名：`pro` → `mimo-v2.5-pro`，`v2.5` → `mimo-v2.5`。

## 配置项

所有运行时配置通过环境变量提供：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MIMO_API_KEY` | 空 | MiMo 上游备用 API Key。正常情况下 Key 来自请求头 `X-Mimo-Api-Key`，此项仅作兜底。 |
| `MIMO_BASE_URL` | `https://token-plan-cn.xiaomimimo.com/v1` | MiMo 上游地址 |
| `MIMO_MODEL` | `mimo-v2.5-pro` | 默认文本模型 |
| `MIMO_PROXY_PORT` | `9876` | 本地监听端口 |
| `MIMO_UPSTREAM_PROTOCOL` | `openai` | 设为 `anthropic` 可切换到 MiMo 的 Anthropic Messages API 上游，适合复杂工具调用场景 |
| `MIMO_PROXY_MAX_CONCURRENT` | `4` | 最大并发上游请求数 |
| `MIMO_PROXY_MIN_INTERVAL_MS` | `600` | 上游请求最小间隔（毫秒） |
| `MIMO_PROXY_429_BACKOFF_MS` | `30000` | 收到上游 `429` 后的退避时间（毫秒） |
| `MIMO_PROXY_LOG` | `mimo_proxy.log` | 日志文件路径 |
| `MIMO_PROXY_SKIP_CC_SWITCH_SYNC` | `false` | 跳过启动同步（开发调试用） |
| `MIMO_PROXY_LEGACY_MODE` | `false` | 恢复更保守的 pre-v1.2 路由默认值 |
| `MIMO_DEBUG_DUMP` | 空 | 设为目录路径后，将上游请求/响应对写入该目录（调试用） |
| `CC_SWITCH_SETTINGS_PATH` | `~/.cc-switch/settings.json` | cc switch 配置文件路径 |
| `CC_SWITCH_DB_PATH` | `~/.cc-switch/cc-switch.db` | cc switch 数据库路径 |
| `CODEX_CONFIG_PATH` | `~/.codex/config.toml` | Codex 配置文件路径 |

## 支持的接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/v1/responses` | 主接口，协议适配的核心入口 |
| `GET` | `/v1/models` | 模型列表 |
| `GET` | `/health` | 健康检查 |
| `POST` | `/v1/responses/compact` | 本地返回不支持的响应 |
| `GET` | `/v1/responses/{id}` | 获取历史响应 |

## 兼容性说明

- function tool 工作流会尽量保留，取决于 MiMo 上游支持情况
- 不兼容的 built-in tool 会被过滤，不会打崩整条请求
- `previous_response_id` 通过有界内存存储实现
- `parallel_tool_calls` 可透传，最终效果取决于 MiMo 上游
- 带图轮次固定回落到 `mimo-v2.5`，纯文本轮次默认保留在 `mimo-v2.5-pro`
- Codex 侧的 GPT 风格模型名会自动映射到 MiMo 后端
- Anthropic 上游模式下，连续同角色消息会在转发前合并为合法的交替格式

## 运行要求

- Go 1.26.3 或更新版本
- 本地已安装 `cc switch`
- `cc switch` 中已配置 Xiaomi MiMo API Key
- 本地 Codex 配置文件 `~/.codex/config.toml` 存在（或通过环境变量指定路径）

## 常见问题

**`cc switch is not installed or incomplete`**

请确认以下文件存在：
- `~/.cc-switch/settings.json`
- `~/.cc-switch/cc-switch.db`
- `~/.codex/config.toml`

**`Xiaomi MiMo provider not found`**

请先在 `cc switch` 中添加 Xiaomi MiMo provider。

**`Xiaomi MiMo API key is empty`**

打开 `cc switch`，编辑 Xiaomi MiMo provider，填入 API Key。

**`当前 provider 不是 Xiaomi MiMo`**

这通常不是问题。CCMimoLink 会自动查找 Xiaomi MiMo provider 并完成同步。

**`cc switch 里的请求地址看起来不对`**

再执行一次 `./ccmimolink sync`，然后重启 `cc switch`。

**`Codex 仍然使用旧路由或旧 Key`**

同步完成后请重启 Codex，让它重新加载配置。

**`为什么必须重启 cc switch 和 Codex？`**

这两个应用会把 provider 配置缓存在内存里。重启后才能确保改写后的路由和新的 `X-Mimo-Api-Key` 生效。

## 版本历史

### v2.0
- **Anthropic Messages 上游模式**：新增 `MIMO_UPSTREAM_PROTOCOL=anthropic`，通过 MiMo 的 Anthropic Messages API 路由，解锁扩展思考和更高的工具调用保真度
- **扩展思考支持**：thinking 块正确流式传输和回放；含工具历史时自动抑制，防止上游卡死
- **工具调用硬化**：六项定向修复——空工具 ID 防护、正确的 `tool_choice` 映射（auto/required/none/function）、多 system message 拼接、scanner 错误恢复、flush 防护、调试文件权限收紧
- **HTTP 超时保护**：`ResponseHeaderTimeout`（60s）+ 空闲流超时（60s），防止停滞的上游占用并发槽
- **并发槽泄漏修复**：流在所有退出路径上都正确释放限流槽，消除高 fan-out 下的冻结 bug

### v1.2
- Codex 子任务兼容性增强、更稳定的多子任务路由、图文混合场景自动回落到 `mimo-v2.5`

### v1.1
- 内置 CLI 子命令，支持模型管理和配置同步

## 许可证

MIT License。详见 [LICENSE](LICENSE)。

## 作者

[@simonlin](https://x.com/simonlin) · [GitHub](https://github.com/SimonLeen22/CCMimoLink)
