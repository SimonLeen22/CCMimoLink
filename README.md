# CCMimoLink

> **[English](README_EN.md)** · **[中文](README_CN.md)** · **[HTML 文档](index.html)**

A local routing bridge that brings Xiaomi MiMo to `cc switch` + Codex.

CCMimoLink is not another reverse proxy — it is an active protocol adapter that translates Codex-style requests into a format MiMo can handle reliably.

```
User → Codex → CCMimoLink (local proxy) → Xiaomi MiMo upstream
         ↑                                       |
         └── cc switch manages API Key ──────────┘
```

## Key Capabilities

- **Protocol Adaptation** — Responses → Chat Completions translation with tool compatibility
- **Multi-turn Continuation** — Bounded in-memory state, `previous_response_id` support
- **Streaming Fidelity** — True incremental streaming, not buffered fake streams
- **Multimodal Fallback** — Image requests auto-fallback to `mimo-v2.5`
- **Dynamic Model Switching** — Switch between `mimo-v2.5` and `mimo-v2.5-pro`
- **Safe Config Sync** — Auto-backup before rewriting, throttling with 429 backoff

## Quick Start

```bash
# 1. Configure Xiaomi MiMo API key in cc switch
# 2. Build
go build -o ccmimolink .

# 3. Sync config only (optional)
./ccmimolink --sync-only

# 4. Run
./ccmimolink
```

Default proxy address: `http://127.0.0.1:9876/v1`

## Documentation

| | |
|---|---|
| English | [README_EN.md](README_EN.md) |
| 中文 | [README_CN.md](README_CN.md) |
| HTML 文档 | [index.html](index.html) |

## Requirements

- Go 1.26.3+
- `cc switch` installed locally
- Xiaomi MiMo API key configured in `cc switch`

## License

MIT License. See [LICENSE](LICENSE).
