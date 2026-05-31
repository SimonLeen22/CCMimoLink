# CCMimoLink

> 面向 `cc switch` + Codex 的 Xiaomi MiMo 本地路由桥接器。

CCMimoLink 不是又一个反向代理。它是一个主动协议适配层——把 Codex 风格的请求翻译成 MiMo 能稳定处理的形式，让你在 Codex 里无缝使用 Xiaomi MiMo 模型。

## 它做了什么

```
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

CCMimoLink 的核心价值在于它不只是转发请求——它主动做协议转换：

- **Responses → Chat Completions**：把 OpenAI 风格的 `/v1/responses` 请求翻译成 MiMo 的 chat-completions 格式，必要时把 `instructions` 注入为 system message
- **Tool 兼容层**：保留标准 function tool，规范化 `tool_choice`，过滤不兼容的 built-in tool 避免整条请求崩掉
- **多轮续接**：通过有界内存保存 response-chain 状态，支持 `previous_response_id`，在多轮交互中回放 function-call 上下文和 provider reasoning 状态
- **流式保真**：保留真实的增量流式路径，在读取 MiMo 上游流时发出 Responses 风格事件

### 智能路由

- **多模态回落**：请求中带图片时，自动回落到 `mimo-v2.5` 保证兼容性
- **动态模型切换**：纯文本请求可在 `mimo-v2.5` 与 `mimo-v2.5-pro` 之间通过环境变量或启动参数动态切换

### 安全与韧性

- **启动安全同步**：回写 Codex 配置前自动备份原始文件
- **限流与退避**：自带请求限流和上游 `429` 退避处理
- **XML 兜底解析**：必要时可以从类 tool-call 文本中恢复工具调用意图
- **本地 compact 处理**：对 `compact` 这类控制面请求提供本地处理，不把错误甩给上游

## 工作流程

```
1. 用户在 cc switch 中配置 Xiaomi MiMo 的 API Key
2. 启动 CCMimoLink
3. CCMimoLink 自动完成：
   ├── 检查 cc switch 是否已安装
   ├── 查找 cc switch 中的 Xiaomi MiMo provider
   ├── 把 MiMo 路由改写为本地代理地址
   ├── 备份并回写本地 Codex 配置
   └── 从 cc switch 刷新 Codex 的 X-Mimo-Api-Key
4. Codex 请求自动走本地 CCMimoLink
5. CCMimoLink 完成协议适配后转发给 MiMo 上游
```

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
./ccmimolink --sync-only
```

### 第四步：启动代理

```bash
./ccmimolink
```

默认本地代理地址：`http://127.0.0.1:9876/v1`

### 第五步：切换模型（可选）

纯文本请求默认使用 `mimo-v2.5`，可通过以下方式切换到 `mimo-v2.5-pro`：

```bash
# 环境变量方式
MIMO_MODEL="mimo-v2.5-pro" ./ccmimolink

# 启动参数方式
./ccmimolink --v2.5-pro
```

> 图片请求不受影响，始终回落到 `mimo-v2.5`。

## 配置项

所有运行时配置通过环境变量提供：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MIMO_API_KEY` | 空 | MiMo 上游备用 API Key。正常情况下 Key 来自请求头 `X-Mimo-Api-Key`，此项仅作兜底。 |
| `MIMO_BASE_URL` | `https://token-plan-cn.xiaomimimo.com/v1` | MiMo 上游地址 |
| `MIMO_MODEL` | `mimo-v2.5` | 默认文本模型 |
| `MIMO_PROXY_PORT` | `9876` | 本地监听端口 |
| `MIMO_PROXY_MAX_CONCURRENT` | `1` | 最大并发上游请求数 |
| `MIMO_PROXY_MIN_INTERVAL_MS` | `1500` | 上游请求最小间隔（毫秒） |
| `MIMO_PROXY_429_BACKOFF_MS` | `30000` | 收到上游 `429` 后的退避时间（毫秒） |
| `MIMO_PROXY_LOG` | `ccmimolink.log` | 日志文件路径 |
| `MIMO_PROXY_SKIP_CC_SWITCH_SYNC` | `false` | 跳过启动同步（开发调试用） |
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
- 图片请求固定回落到 `mimo-v2.5`

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

再执行一次 `./ccmimolink --sync-only`，然后重启 `cc switch`。

**`Codex 仍然使用旧路由或旧 Key`**

同步完成后请重启 Codex，让它重新加载配置。

**`为什么必须重启 cc switch 和 Codex？`**

这两个应用会把 provider 配置缓存在内存里。重启后才能确保改写后的路由和新的 `X-Mimo-Api-Key` 生效。

## 许可证

MIT License。详见 [LICENSE](LICENSE)。
