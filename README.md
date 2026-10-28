# searchbridge

<p align="center"><b>简体中文</b> • <a href="README_EN.md">English</a></p>

给任意 Anthropic Messages 端点**补上原生 `web_search`** 的本地网关：多搜索后端链 + 自动故障转移 + 结果精选。

ZCode / Claude Code 等客户端调用联网搜索时，会发出一个只携带 `web_search` 服务端工具的旁路请求（side request），由模型提供方执行搜索。很多自定义模型端点（例如 StepFun）并不支持这个服务端工具；而 [wbmux](https://github.com/HMuSeaB/wbmux) 这类工具虽然提供搜索端点，却只处理搜索请求、不能当主端点用。

searchbridge 把两者接起来，并且不依赖任何单一搜索来源：

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
                 │   搜索后端链 + 精选层          │
                 │  remote → ddg → bing → ...   │
                 │  故障转移 · 共识加权 · 垃圾过滤  │
                 └─────────────────────────────┘
```

- **普通对话**：字节级透传转发到上游模型端点，流式 / 非流式均原样返回，客户端自带的认证头会被替换为你配置的上游 Key。
- **联网搜索**：识别请求中的 `web_search` 服务端工具，按优先级依次尝试搜索后端；某个后端失败后进入 10 分钟冷却期自动跳过，全部失败才报错。结果合成为标准 Anthropic Message（流式请求则合成 SSE：`message_start` → `server_tool_use` → `web_search_tool_result` → `message_delta` → `message_stop`），客户端按流式直接消费。
- **结果精选**：搜索引擎返回的结果里混有大量垃圾（SEO 农场、赌博站、不相关内容），直接喂给模型会污染答案。精选层在链路拿到结果后做四件事：垃圾特征过滤（赌博/色情等强特征词）、相关性评分与分级（查询词在标题/摘要/URL 的覆盖度，CJK 查询用二元组分词）、去重（规范化 URL + 规范化标题）、排序。全垃圾的结果会触发下一个后端，但**不**进入冷却（换个查询它可能就正常）。
- **合并模式**：`SB_SEARCH_MERGE=1` 时并行查询多个后端（默认 3 个），被多个引擎同时返回的结果获得共识加权——跨引擎共识是抵御单一引擎垃圾/偏见的最有效信号。
- **结构化 JSON 端点**：`GET/POST /v1/search?q=关键词&n=5`，与 wbmux 的 `/v1/search` 格式兼容，附带精选统计（`stats: {total, good, partial, junk, deduped}`），可直接给脚本、MCP、Raycast 之类用。

零第三方依赖，单二进制，全部配置通过环境变量完成。

## 快速开始

```bash
go build -o searchbridge .

export SB_LISTEN="127.0.0.1:8787"
export SB_UPSTREAM_URL="https://api.stepfun.com/step_plan"   # 普通聊天转发目标
export SB_UPSTREAM_API_KEY="sk-..."                          # 上游 Key（x-api-key）
# 搜索后端链默认 remote,ddg,bing，零配置即可用

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

### 基础

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SB_LISTEN` | `127.0.0.1:8787` | 监听地址 |
| `SB_UPSTREAM_URL` | 空 | 普通聊天转发目标（Anthropic Messages 兼容端点） |
| `SB_UPSTREAM_API_KEY` | 空 | 上游 Key，以 `x-api-key` 头发送 |
| `SB_UPSTREAM_BEARER` | 空 | 设为 `1` 时上游 Key 改以 `Authorization: Bearer` 发送 |

### 搜索后端链

`SB_SEARCH_BACKENDS` 定义后端及优先级（逗号分隔，顺序即尝试顺序），默认 `remote,ddg,bing`：

| 后端 | 费用 | 说明 | 相关配置 |
|---|---|---|---|
| `remote` | 免费* | wbmux 等 Anthropic 搜索端点（复用已登录凭据） | `SB_SEARCH_URL`、`SB_SEARCH_API_KEY`、`SB_SEARCH_MODEL` |
| `ddg` | 免费 | DuckDuckGo Lite，无需 Key（海外可达） | `SB_DDG_URL` |
| `bing` | 免费 | Bing 网页搜索，国内默认走 `cn.bing.com` | `SB_BING_HOST` |
| `searxng` | 免费 | 自建 / 公共 SearXNG 实例（需开启 JSON API） | `SB_SEARXNG_URL` |
| `tavily` | 免费额度 1000 次/月 | Tavily Search API | `SB_TAVILY_API_KEY`、`SB_TAVILY_URL` |
| `brave` | 付费 | Brave Search API（2026 年起已无免费档，$5/1000 次） | `SB_BRAVE_API_KEY`、`SB_BRAVE_URL` |

\* remote 免费的前提是 wbmux 桌面端在运行。

其他搜索配置：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SB_SEARCH_URL` | `http://127.0.0.1:55464/v1/search/messages` | remote 后端地址；设为 `off` 可禁用 |
| `SB_SEARCH_API_KEY` | 空 | remote 后端 Key（wbmux 本机回环免密） |
| `SB_SEARCH_MODEL` | `web-search` | 请求 remote 时使用的模型名 |
| `SB_SEARCH_COOLDOWN_MIN` | `10` | 后端失败后的冷却分钟数 |
| `SB_SEARCH_CACHE_MIN` | `30` | 搜索结果缓存分钟数（同查询同条数直接命中） |
| `SB_SEARCH_MERGE` | 空 | 设为 `1` 开启合并模式：多后端并行查询 + 共识加权 |
| `SB_SEARCH_MERGE_WIDTH` | `3` | 合并模式同时查询的后端数 |
| `SB_SEARCH_HEDGE_MS` | `1000` | 对冲窗口（毫秒）：首后端超时未果即并行启动下一个，先出可用结果者胜出；设为 `0` 关闭，退回串行 failover |

示例——只用免费后端，searxng 优先：

```bash
export SB_SEARCH_BACKENDS="searxng,ddg,bing"
export SB_SEARXNG_URL="http://127.0.0.1:8080"
```

示例——合并模式（三引擎并行，共识加权，质量优先）：

```bash
export SB_SEARCH_MERGE=1
export SB_SEARCH_MERGE_WIDTH=3
```

示例——对冲模式（默认开启，延迟优先）：链首放最全的源，链尾放最快的源，
窗口内没有结果就并行竞速：

```bash
export SB_SEARCH_BACKENDS="remote,ddg,bing"   # 默认即为对冲模式
export SB_SEARCH_HEDGE_MS=1000                # 首后端 1s 未返回就启动 ddg
```

### 三种链路模式

| 模式 | 触发条件 | 行为 | 适用 |
|---|---|---|---|
| 合并（merge） | `SB_SEARCH_MERGE=1` | 固定并行查询 N 个后端，共识加权合并全部结果 | 质量优先，不在乎多打几次上游 |
| 对冲（hedge） | `SB_SEARCH_HEDGE_MS>0`（默认 1000） | 串行起步，首后端超过窗口未返回即并行启动下一个，**先出可用结果者胜出**，落败方取消 | 延迟优先（默认） |
| 串行（failover） | `SB_SEARCH_HEDGE_MS=0` | 一个失败/全垃圾才试下一个 | 配额敏感（每个上游只打一次） |

对冲的延迟收益来自链路延迟分布的长尾：链首源（如 wbmux）p99 数秒时，
链尾亚秒级源胜出的概率很高，端到端延迟从「链首全延迟」压到「窗口 + 次后端
延迟」。实测（本机网络，链 `remote,ddg,bing`）：串行时 remote 单独
3.8–4.2s；对冲模式下胜出耗时 0.8–2.5s。注意代价是可能多打一次上游请求——
按量计费的后端（tavily/brave）落败方也可能计费，把它们排在链尾。

后端出错或返回全垃圾时**立刻补位**（这是 failover，不等对冲计时器）；
全垃圾不进入冷却，错误与空结果才冷却。

## 精选层

搜索引擎返回的原始结果里混有大量垃圾——SEO 农场、赌博站、与查询无关的内容页。直接把这些喂给模型，答案会被污染。精选层在链路拿到每个后端的结果后依次做四件事：

1. **垃圾特征过滤**：标题/摘要/URL 命中赌博、色情、诱导注册等强特征词的结果直接丢弃（特征库刻意保守，宁可漏判不误伤）。
2. **相关性评分与分级**：查询分词为完整短语（权重 3）、拉丁/数字整词与 CJK 整段（权重 2）、CJK 二元组（权重 1），按在标题/摘要/URL 的命中计算覆盖率。`good` = 覆盖率 ≥ 0.5 或主词全部命中；`partial` = 有主词命中但覆盖率不足；`junk` = 完全无命中，或**只命中二元组**（「特性」这类弱信号跨语句误配概率高，不足以构成相关），丢弃。
3. **去重**：规范化 URL（去 scheme/www/query/末尾斜杠，保留路径）精确去重 + 标题**近似去重**——跨引擎常对同一页面给出不同 URL 但标题是详略变体（长的多一个「（完整版）」之类后缀），按标题 token 集合的包含度（较小集合被覆盖 ≥ 0.85，少于 3 个 token 的短标题不判定）判重，只保留一份。
4. **排序**：相关分降序，叠加三个质量信号后重排，`good` 优先、`partial` 仅在优质结果不足 limit 时兜底填充：
   - **权威域名加权 ×1.25**：语言/框架/数据库/云厂商的官方文档与源码库、Stack Overflow 等公认第一方来源（go.dev、docs.python.org、github.com、developer.mozilla.org……）。只加权不过滤——漏杀一个真实域名的代价远大于漏加分，且列表不可能穷尽。
   - **时效加权**：查询含时效意图（「最新」「latest」「近期」……）时，结果文本提及当前年或去年 ×1.2、只提及更早年份 ×0.85。问「最新」时不该被三年前的教程占位。
   - **跨引擎共识**（合并模式）：被 k 个后端同时返回的结果 ×(1+0.5(k-1))。

链路行为：某后端返回的结果**全垃圾**时触发下一个后端，但不进入冷却（该后端对其他查询可能仍然可用）；错误与空结果才进入冷却。合并模式下各后端的结果按 URL 归并，共识加成同样生效。

## 路由判定

请求 `tools` 数组中任意工具满足以下任一条件即判定为联网搜索请求：

- `name == "web_search"`（Anthropic 服务端工具的标准命名）
- `type` 以 `web_search_` 开头（如 `web_search_20250305`、`web_search_20260209`）

客户端自定义工具（如带 `input_schema` 的 `WebSearch`）不受影响，仍按普通聊天处理。

## 端点

| 路径 | 行为 |
|---|---|
| `POST /v1/messages` | 按路由判定分流（聊天 / 搜索） |
| `POST /v1/messages/count_tokens` | 转发上游 |
| `GET /v1/models` | 转发上游 |
| `GET/POST /v1/search?q=&n=` | 结构化 JSON 搜索，走后端链 |
| `GET /healthz` | 健康检查，返回当前路由与后端链 |

## 开发

```bash
go test ./...   # 运行测试（后端解析、链路故障转移/冷却/缓存、路由、SSE 合成）
go vet ./...    # 静态检查
```

## 许可

MIT
