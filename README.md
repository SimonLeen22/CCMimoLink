# CCMimoLink

<p align="center">
  <strong>Xiaomi MiMo Local Routing Bridge for <code>cc switch</code> + Codex</strong><br>
  <strong>面向 <code>cc switch</code> + Codex 的 Xiaomi MiMo 本地路由桥接器</strong>
</p>

<p align="center">
  <a href="https://simonleen22.github.io/CCMimoLink/">🌐 Open the full HTML homepage / 打开完整 HTML 首页</a>
</p>

## Why CCMimoLink

CCMimoLink is not another reverse proxy. It is an active protocol adapter that translates Codex-style requests into a format Xiaomi MiMo can handle reliably.

CCMimoLink 不只是一个普通反向代理。它是一个主动协议适配层，把 Codex 风格的请求翻译成 MiMo 能稳定处理的形式。

```text
User -> Codex -> CCMimoLink (local proxy) -> Xiaomi MiMo upstream
         ^                                      |
         |-- cc switch manages API Key ---------|
```

## Key Capabilities / 核心能力

- **Protocol Adaptation / 协议适配** — OpenAI-style Responses API to MiMo chat completions.
- **cc switch Integration / cc switch 集成** — reads Xiaomi MiMo API key from cc switch and rewrites local routing.
- **Codex Compatibility / Codex 兼容** — function tools, `previous_response_id`, `parallel_tool_calls`, compact handling, and true streaming.
- **Multimodal Fallback / 多模态回落** — image requests automatically use `mimo-v2.5`.
- **Dynamic Model Switching / 动态模型切换** — switch text requests between `mimo-v2.5` and `mimo-v2.5-pro`.
- **Production-minded Reliability / 生产级可靠性** — request throttling and upstream `429` backoff.

## Quick Start / 快速开始

```bash
# 1. Configure Xiaomi MiMo API key in cc switch
# 2. Build
go build -o ccmimolink .

# 3. Sync config only
./ccmimolink --sync-only

# 4. Run
./ccmimolink
```

Default proxy address / 默认代理地址:

```text
http://127.0.0.1:9876/v1
```

## Technical Adaptation / 技术适配细节

- Accepts OpenAI-style `/v1/responses` and adapts requests to MiMo chat completions.
- Preserves function tool workflows and normalizes `tool_choice`.
- Filters unsupported built-in tools instead of crashing upstream requests.
- Supports bounded in-memory continuation for `previous_response_id`.
- Replays function-call context and provider reasoning state across turns.
- Keeps true incremental streaming rather than buffered fake streams.
- Handles `/v1/responses/compact` locally.
- Parses XML-style tool-call fallback output when needed.

## Startup Sync / 启动同步

On startup, CCMimoLink:

- checks that `cc switch` is installed;
- locates the Xiaomi MiMo Codex provider, even if it is not the currently selected provider;
- requires the Xiaomi MiMo API key to already exist in cc switch;
- rewrites the Xiaomi MiMo route in cc switch to the local proxy URL;
- backs up and rewrites `~/.codex/config.toml`;
- refreshes `X-Mimo-Api-Key` from cc switch;
- reminds the user to restart cc switch and Codex.

启动时，CCMimoLink 会：

- 检查 `cc switch` 是否已安装；
- 自动查找 Xiaomi MiMo Codex provider，即使当前选中的不是它；
- 要求用户已经在 `cc switch` 里填好 Xiaomi MiMo API Key；
- 把 cc switch 里的 Xiaomi MiMo 请求地址改写成本地代理；
- 备份并回写 `~/.codex/config.toml`；
- 从 cc switch 刷新 `X-Mimo-Api-Key`；
- 提示用户重启 cc switch 和 Codex。

## Configuration / 配置项

| Variable | Default | Description |
| --- | --- | --- |
| `MIMO_API_KEY` | empty | Fallback MiMo upstream API key. Normally the key comes from `X-Mimo-Api-Key`. |
| `MIMO_BASE_URL` | `https://token-plan-cn.xiaomimimo.com/v1` | MiMo upstream base URL. |
| `MIMO_MODEL` | `mimo-v2.5` | Default text model. |
| `MIMO_PROXY_PORT` | `9876` | Local listen port. |
| `MIMO_PROXY_MAX_CONCURRENT` | `1` | Maximum concurrent upstream requests. |
| `MIMO_PROXY_MIN_INTERVAL_MS` | `1500` | Minimum delay between upstream requests. |
| `MIMO_PROXY_429_BACKOFF_MS` | `30000` | Backoff after upstream `429`. |
| `MIMO_PROXY_LOG` | `ccmimolink.log` | Log file path. |
| `MIMO_PROXY_SKIP_CC_SWITCH_SYNC` | `false` | Skip startup sync during development. |
| `CC_SWITCH_SETTINGS_PATH` | `~/.cc-switch/settings.json` | Override cc switch settings path. |
| `CC_SWITCH_DB_PATH` | `~/.cc-switch/cc-switch.db` | Override cc switch database path. |
| `CODEX_CONFIG_PATH` | `~/.codex/config.toml` | Override Codex config path. |

## Requirements / 运行要求

- Go 1.26.3+
- `cc switch` installed locally
- Xiaomi MiMo API key configured in `cc switch`
- Codex config at `~/.codex/config.toml`

## Troubleshooting / 常见问题

- **`cc switch is not installed or incomplete`** — verify `~/.cc-switch/settings.json`, `~/.cc-switch/cc-switch.db`, and `~/.codex/config.toml`.
- **`Xiaomi MiMo provider not found`** — add Xiaomi MiMo provider in cc switch first.
- **`Xiaomi MiMo API key is empty`** — edit Xiaomi MiMo provider in cc switch and input the API key.
- **Codex still uses the old route or key** — restart Codex after sync.
- **cc switch route still looks incorrect** — run `./ccmimolink --sync-only` again, then restart cc switch.

## License

MIT License. See [LICENSE](LICENSE).
