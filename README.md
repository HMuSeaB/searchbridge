# searchbridge

给任意 Anthropic Messages 端点**补上原生 `web_search`** 的本地网关。

ZCode / Claude Code 等客户端调用联网搜索时，会发出一个只携带 `web_search` 服务端工具的旁路请求（side request），由模型提供方执行搜索。很多自定义模型端点（例如 StepFun）并不支持这个服务端工具；而 [wbmux](https://github.com/HMuSeaB/wbmux) 这类工具虽然提供搜索端点，却只处理搜索请求、不能当主端点用。

searchbridge 把两者接起来：

```
                 ┌─────────────────────────────┐
 ZCode / Claude  │ 普通聊天请求                  │
 Code            │   （无 web_search 工具）      │
 (anthropic-     │             │                │
  messages)      │             ▼                │
                 │      上游模型端点             │
                 │      （如 StepFun）           │
                 │                             │
                 │ 联网搜索旁路请求              │
                 │   （带 web_search 工具）      │
                 │             │                │
                 │             ▼                │
                 │      搜索端点                 │
                 │      （如 wbmux）             │
                 └─────────────────────────────┘
```

- **普通对话**：字节级透传转发到上游模型端点，流式 / 非流式均原样返回，客户端自带的认证头会被替换为你配置的上游 Key。
- **联网搜索**：识别请求中的 `web_search` 服务端工具，转发到搜索端点；搜索端点通常只返回 JSON（即使请求了 `stream:true`），searchbridge 会把它合成为标准 Anthropic SSE 事件流（`message_start` → `server_tool_use` → `web_search_tool_result` → `text`/`citations` → `message_delta` → `message_stop`），按流式解析的客户端可以直接消费。

零第三方依赖，单二进制，全部配置通过环境变量完成。

## 快速开始

```bash
go build -o searchbridge .

export SB_LISTEN="127.0.0.1:8787"
export SB_UPSTREAM_URL="https://api.stepfun.com/step_plan"   # 普通聊天转发目标
export SB_UPSTREAM_API_KEY="sk-..."                          # 上游 Key（x-api-key）
export SB_SEARCH_URL="http://127.0.0.1:55464/v1/search/messages"  # wbmux 搜索端点

./searchbridge
```

然后把客户端的模型端点指向 searchbridge，并在模型配置里打开「原生联网搜索 / Native web search」。

### ZCode

设置 → 模型供应商 → 自定义模型：

| 配置项 | 值 |
|---|---|
| API 地址（Base URL） | `http://127.0.0.1:8787` |
| API Key | 任意值（会被替换为 `SB_UPSTREAM_API_KEY`） |
| 模型能力 → 原生联网搜索 | **开启** |

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_API_KEY="dummy"
claude
```

## 配置项

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SB_LISTEN` | `127.0.0.1:8787` | 监听地址 |
| `SB_UPSTREAM_URL` | 空 | 普通聊天转发目标（Anthropic Messages 兼容端点） |
| `SB_UPSTREAM_API_KEY` | 空 | 上游 Key，以 `x-api-key` 头发送 |
| `SB_UPSTREAM_BEARER` | 空 | 设为 `1` 时上游 Key 改以 `Authorization: Bearer` 发送 |
| `SB_SEARCH_URL` | `http://127.0.0.1:55464/v1/search/messages` | 搜索端点 |
| `SB_SEARCH_API_KEY` | 空 | 搜索端点 Key（wbmux 本机回环免密，可留空） |

## 路由判定

请求 `tools` 数组中任意工具满足以下任一条件即判定为联网搜索请求，转发至搜索端点：

- `name == "web_search"`（Anthropic 服务端工具的标准命名）
- `type` 以 `web_search_` 开头（如 `web_search_20250305`、`web_search_20260209`）

客户端自定义工具（如带 `input_schema` 的 `WebSearch`）不受影响，仍按普通聊天处理。

## 端点

| 路径 | 行为 |
|---|---|
| `POST /v1/messages` | 按路由判定分流 |
| `POST /v1/messages/count_tokens` | 转发上游 |
| `GET /v1/models` | 转发上游 |
| `GET /healthz` | 健康检查，返回当前路由配置 |

## 开发

```bash
go test ./...   # 运行测试
go vet ./...    # 静态检查
```

## 许可

MIT

---

## English

**searchbridge** is a local gateway that adds native `web_search` support to any Anthropic Messages-compatible endpoint.

When ZCode / Claude Code perform a web search, they send a side request carrying only the `web_search` server tool, expecting the model provider to execute the search. Many custom endpoints (e.g. StepFun) don't support that server tool, while tools like [wbmux](https://github.com/HMuSeaB/wbmux) provide a search endpoint that only handles search requests and can't serve as the main endpoint.

searchbridge bridges the two:

- **Chat requests** (no `web_search` tool) are forwarded byte-for-byte to your upstream model endpoint, streaming or not. Client-supplied auth headers are replaced with your configured upstream key.
- **Search requests** (with a `web_search` tool) are forwarded to the search endpoint. Since search endpoints usually return plain JSON even when streaming was requested, searchbridge synthesizes the standard Anthropic SSE event sequence so streaming clients can consume it directly.

Zero dependencies, single binary, configured entirely via environment variables — see the table above.

```bash
go build -o searchbridge .
SB_UPSTREAM_URL="https://api.stepfun.com/step_plan" \
SB_UPSTREAM_API_KEY="sk-..." \
./searchbridge
# point ANTHROPIC_BASE_URL / your custom model base URL at http://127.0.0.1:8787
```

MIT licensed.
