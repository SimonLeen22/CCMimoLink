# CCMimoLink

> A local routing bridge that brings Xiaomi MiMo to `cc switch` + Codex.

CCMimoLink is not another reverse proxy. It is an active protocol adapter that translates Codex-style requests into a format MiMo can handle reliably — letting you use Xiaomi MiMo models seamlessly within Codex.

## What It Does

```
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

CCMimoLink's core value lies in active protocol translation, not just request forwarding:

- **Responses → Chat Completions**: Translates OpenAI-style `/v1/responses` into MiMo's chat-completions format, injecting `instructions` as a system message when needed.
- **Tool Compatibility Layer**: Preserves standard function tools, normalizes `tool_choice`, and filters unsupported built-in tools to prevent upstream failures.
- **Multi-turn Continuation**: Maintains bounded in-memory state for response chains, supports `previous_response_id`, and replays function-call context and provider reasoning across turns.
- **Streaming Fidelity**: Preserves true incremental streaming, emitting Responses-style events while reading from the MiMo upstream stream.

### Intelligent Routing

- **Multimodal Fallback**: Requests containing images automatically fall back to `mimo-v2.5` for compatibility.
- **Dynamic Model Switching**: Text requests can switch between `mimo-v2.5` and `mimo-v2.5-pro` via environment variables or startup flags.

### Safety and Resilience

- **Safe Startup Sync**: Automatically backs up Codex config before rewriting.
- **Throttling and Backoff**: Built-in request rate limiting with upstream `429` retry backoff.
- **XML Fallback Parsing**: Recovers tool-call intent from tool-call-like text output when needed.
- **Local Compact Handling**: Handles `compact` control-plane requests locally instead of letting them fail upstream.

## How It Works

```
1. User configures the Xiaomi MiMo API key in cc switch
2. CCMimoLink starts
3. On startup, CCMimoLink automatically:
   ├── Verifies cc switch is installed
   ├── Locates the Xiaomi MiMo provider in cc switch
   ├── Rewrites the MiMo route to the local proxy URL
   ├── Backs up and rewrites the local Codex config
   └── Refreshes X-Mimo-Api-Key in Codex from cc switch
4. Codex requests are routed through the local CCMimoLink proxy
5. CCMimoLink adapts the protocol and forwards to MiMo upstream
```

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
./ccmimolink --sync-only
```

### Step 4: Run the proxy

```bash
./ccmimolink
```

Default local proxy address: `http://127.0.0.1:9876/v1`

### Step 5: Switch models (optional)

Text requests default to `mimo-v2.5`. Switch to `mimo-v2.5-pro` with either method:

```bash
# Environment variable
MIMO_MODEL="mimo-v2.5-pro" ./ccmimolink

# Startup flag
./ccmimolink --v2.5-pro
```

> Image requests are unaffected and always fall back to `mimo-v2.5`.

## Configuration

All runtime settings are provided via environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `MIMO_API_KEY` | empty | Fallback MiMo upstream API key. Normally the key comes from the `X-Mimo-Api-Key` header; this is only a safety net. |
| `MIMO_BASE_URL` | `https://token-plan-cn.xiaomimimo.com/v1` | MiMo upstream base URL. |
| `MIMO_MODEL` | `mimo-v2.5` | Default text model. |
| `MIMO_PROXY_PORT` | `9876` | Local listen port. |
| `MIMO_PROXY_MAX_CONCURRENT` | `1` | Maximum concurrent upstream requests. |
| `MIMO_PROXY_MIN_INTERVAL_MS` | `1500` | Minimum delay between upstream requests (ms). |
| `MIMO_PROXY_429_BACKOFF_MS` | `30000` | Backoff duration after an upstream `429` response (ms). |
| `MIMO_PROXY_LOG` | `ccmimolink.log` | Log file path. |
| `MIMO_PROXY_SKIP_CC_SWITCH_SYNC` | `false` | Skip startup sync (for development). |
| `CC_SWITCH_SETTINGS_PATH` | `~/.cc-switch/settings.json` | Path to cc switch settings file. |
| `CC_SWITCH_DB_PATH` | `~/.cc-switch/cc-switch.db` | Path to cc switch database. |
| `CODEX_CONFIG_PATH` | `~/.codex/config.toml` | Path to local Codex config. |

## Supported Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/responses` | Primary endpoint — the core protocol adaptation entry point. |
| `GET` | `/v1/models` | Model listing. |
| `GET` | `/health` | Health check. |
| `POST` | `/v1/responses/compact` | Returns a local unsupported response. |
| `GET` | `/v1/responses/{id}` | Retrieve a stored response by ID. |

## Compatibility Notes

- Function tool workflows are preserved to the extent MiMo supports them.
- Unsupported built-in tools are filtered rather than crashing the entire request.
- `previous_response_id` continuation is implemented via bounded in-memory storage.
- `parallel_tool_calls` is accepted and forwarded; final behavior depends on MiMo upstream support.
- Image requests are always pinned to `mimo-v2.5`.

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

Run `./ccmimolink --sync-only` again, then restart `cc switch`.

**`Codex still uses the old route or key`**

Restart Codex after sync so it reloads `~/.codex/config.toml`.

**`Why do I need to restart cc switch and Codex?`**

Both applications cache provider configuration in memory. Restarting ensures the rewritten route and refreshed `X-Mimo-Api-Key` take effect.

## License

MIT License. See [LICENSE](LICENSE).
