# CCMimoLink

A local routing bridge that brings Xiaomi MiMo to `cc switch` + Codex.

CCMimoLink is not another reverse proxy — it is an active protocol adapter that translates Codex-style requests into a format MiMo can handle reliably.

```
User -> Codex -> CCMimoLink (local proxy) -> Xiaomi MiMo upstream
         ^                                      |
         |-- cc switch manages API Key ---------|
```

## Documentation

- Open [README.html](README.html) in a browser for the full bilingual documentation.

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

## Key Capabilities

- Protocol adaptation: OpenAI-style Responses API to MiMo chat completions
- cc switch integration: reads Xiaomi MiMo API key from cc switch and rewrites local routing
- Codex compatibility: function tools, continuation, compact handling, true streaming
- Multimodal fallback: image requests automatically use `mimo-v2.5`
- Dynamic model switching: `mimo-v2.5` and `mimo-v2.5-pro`
- Production-minded behavior: throttling and upstream `429` backoff

## Requirements

- Go 1.26.3+
- `cc switch` installed locally
- Xiaomi MiMo API key configured in `cc switch`

## License

MIT License. See [LICENSE](LICENSE).
