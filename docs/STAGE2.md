# 阶段 2 执行计划：basispoints 直转（不带工具）

> 这份文档是阶段 2 的**工作底稿**：先写计划，再按清单执行，每完成一步就勾选并记录结果。
> 上下文被压缩后，从「执行清单」里第一个没勾的步骤继续即可。
> 总体路线见 [PLAN.md](PLAN.md)。

## 0. 目标与范围

**目标**：满足条件的 OAuth 请求由插件改写后发往
`https://bps.openai.com/basispoints/api/responses`，其余请求照旧原样发往 codex。

**本阶段做**：路由判定、请求头改写、请求体改写、失败回落 codex、状态统计、配置项、测试环境实测。

**本阶段不做**（阶段 3）：工具中转（带 `tools` 的请求一律走 codex）、图片上传（带图片的请求走 codex）。

## 1. 已确认的事实（写代码的依据）

### 1.1 basispoints 端点（2026-09-25 用测试环境账号 2 的 OAuth 令牌实测）

- 能用：`gpt-5.6-sol` / `gpt-5.6-terra` / `gpt-5.6-luna` / `gpt-6-astra`。
- 不能用：`gpt-6-luna`、`gpt-5.5` → 403 `basispoints_model_access_changed`。
- 推理强度：`low`/`medium`/`high`/`xhigh` 可用，`max` → 422。
- 带客户端 `tools` → 422。
- `reasoning:{effort}`、`instructions`、省略 `model_selection` 都兼容（200）。
- 连接特征：服务器上 `curl` 成功；Python `urllib` 被 Cloudflare 403（HTML 页面）。**Go net/http 能否通过还没测**，这是第一个风险关卡（步骤 S1）。

实测成功的请求头（除令牌外全部固定值）：

```
authorization: Bearer <token>
chatgpt-account-id: <acct>          x-openai-account-id: <acct>
x-basispoints-auth-mode: chatgpt
x-openai-internal-basispoints-client-agent-profile: excel
x-openai-internal-basispoints-client-editor: excel
x-openai-internal-basispoints-client-host: office
x-openai-internal-basispoints-client-platform: excel
x-openai-internal-basispoints-client-platform-class: PC
x-openai-internal-basispoints-client-product: basispoints-excel-plugin
x-openai-internal-basispoints-client-runtime: desktop
x-openai-internal-basispoints-office-host: Excel
x-openai-internal-basispoints-office-platform: PC
x-stainless-arch: unknown   x-stainless-lang: js   x-stainless-os: Unknown
x-stainless-package-version: 6.31.0   x-stainless-retry-count: 0
x-stainless-runtime: browser:chrome
accept: text/event-stream   accept-encoding: identity
content-type: application/json   origin: https://bps.openai.com
user-agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0
```

实测成功的请求体：

```json
{"model":"gpt-5.6-sol","model_selection":"explicit","stream":true,"store":false,
 "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]}],
 "reasoning_effort":"low",
 "context_management":[{"type":"compaction","compact_threshold":200000}],
 "metadata":{"agent_iteration":"1","task_id":"<uuid>","turn_id":"<uuid>"}}
```

### 1.2 sub2api 0.2.8 交给插件的请求（读源码 `openai_gateway_forward.go` `buildUpstreamRequest`、`openai_codex_transform.go`）

- URL 固定 `https://chatgpt.com/backend-api/codex/responses`，compact 请求带 `/compact` 后缀；
  另有 `/alpha/search`、`/models`、`/wham/*` 等其它路径，**全部原样透传**。
- `Host: chatgpt.com`；头里有 `Authorization`、`chatgpt-account-id`、`originator`、`version`、
  `session_id`、`conversation_id`、`user-agent`（Codex 身份）、`OpenAI-Beta`、`x-codex-*` 等。
- 请求体：宿主已强制 `store:false`、`stream:true`（非 compact）；带 reasoning 时补
  `include:["reasoning.encrypted_content"]`；`system` 消息已被移进 `instructions`；
  可能带 `prompt_cache_key`、`client_metadata`、`tools`、`tool_choice`、`parallel_tool_calls`、`text`。
- 用户在 sub2api 看到的模型名经账号 `model_mapping` 映射后才进入请求体。测试环境两个账号的映射里
  已有 `gpt-5.6-sol`、`gpt-6-astra` 等；`gpt-5.6-sol-bps` 这类名字当前返回 404（未映射）。
- **`gpt-5.6-sol`、`gpt-6-astra` 在 codex 端点也能用**（实测 200），所以模型名本身不决定走哪条路。

### 1.3 参考实现（excel-codex-bridge `excel_upstream.py` `prepare_responses_body`）要点

- `instructions` → 放到 `input` 最前面的 `developer` 消息；再加一条提示，让模型别调用 Excel 工具：
  `This request is relayed by an external OpenAI Responses API client, not by the live Excel workbook. Do not call server-injected Excel, Office, connector, or workbook tools. Return the answer as assistant text.`
- `input` 条目：去掉 `internal_chat_message_metadata_passthrough`；`reasoning` 只保留带
  `encrypted_content` 的（改写成 `{"type":"reasoning","summary":[],"encrypted_content":...}`）；
  丢掉 `item_reference`；字符串 input 包成一条 user 消息。
- `reasoning_effort`：取 `reasoning.effort` 或 `reasoning_effort`，`x-high`/`extra-high` → `xhigh`，
  不认识的值 → `medium`。
- `prompt_cache_key` 保留。`context_management` 缺省时补默认值。
- `metadata`：客户端的标量字段保留（key ≤64、value ≤512），另补 `agent_iteration`、`task_id`、`turn_id`，
  **确定性派生**（uuid5），同一会话 task_id 不变、同一轮 turn_id 不变：
  - 会话标识 = `prompt_cache_key` → `client_metadata.session_id` → 第一条 input 的 sha256；
  - turn 指纹 = 截至最后一条 user 消息的 input 前缀的 sha256；
  - `agent_iteration` = 最后一条 user 消息之后工具结果的轮数 + 1。
- 图片：basispoints 拒收 user 消息里的内联图片，参考实现会先上传到 `/basispoints/api/attachments`。本阶段不做，有图片就走 codex。

## 2. 设计

### 2.1 路由判定（`internal/basispoints/route.go`）

一个请求**同时满足**以下全部条件才改走 basispoints，否则原样透传 codex，并记录原因（计数）：

| # | 条件 | 不满足时的原因码 |
|---|---|---|
| 1 | 配置 `enabled = true` | `disabled` |
| 2 | `POST` 且 URL 路径恰好是 `/backend-api/codex/responses`（不含 `/compact`） | `not_responses` |
| 3 | 请求体是合法 JSON 对象，且不超过 `max_body_bytes` | `bad_body` / `body_too_large` |
| 4 | 模型在白名单内（按路由模式处理后缀，见下） | `model_not_allowed` |
| 5 | 没有 `tools`（或为空数组） | `has_tools` |
| 6 | input 里没有图片（`input_image`、`image_url` 之类） | `has_image` |

**路由模式** `route_mode`：
- `all`（默认）：白名单里的模型全部走 basispoints。
- `suffix`：只有带后缀的模型（默认 `-bps`，如 `gpt-5.6-sol-bps`）走 basispoints，发出前去掉后缀；
  不带后缀的照旧走 codex。需要管理员在账号 `model_mapping` 里加 `gpt-5.6-sol-bps → gpt-5.6-sol-bps`。
  这个模式让用户**自己选**走哪条线；**在 S7 实测宿主是否放行这种模型名**，不放行就在文档注明只支持 `all`。

### 2.2 请求改写（`internal/basispoints/rewrite.go`）

- **头**：新建一份干净的请求头，只从原请求带过去 `Authorization`、`chatgpt-account-id`；
  `x-openai-account-id` 取同一个值；加上 1.1 的整套固定头；`user-agent` 可配置。
  `Host`/URL 改为 `bps.openai.com`。原请求的 `originator`、`version`、`session_id`、
  `conversation_id`、`OpenAI-Beta`、`x-codex-*` 全部不带。缺少 `chatgpt-account-id` → 不改写，走 codex（原因码 `no_account_id`）。
- **体**：按 1.3 生成。从原体保留：`model`（去后缀）、`input`（改写后）、`stream`、`prompt_cache_key`、
  `context_management`、`metadata`。固定：`model_selection:"explicit"`、`store:false`。
  不带过去：`instructions`（已移进 input）、`reasoning`、`include`、`tools`、`tool_choice`、
  `parallel_tool_calls`、`text`、`client_metadata`、`service_tier`、`truncation`、`user` 及其它未知字段。
  （这是白名单做法：只发实测过的字段，避免 422。`text.format` 结构化输出本阶段不支持，丢掉。）
- 推理强度：`none`/`minimal` → `low`；`max` → `xhigh`；没给 → `medium`。

### 2.3 失败回落（`fallback_to_codex`，默认开）

basispoints 返回下列情况时，**在给宿主回任何数据之前**，改用原始请求重新发往 codex：
- 403 且 `error.code = basispoints_model_access_changed`（模型权限变化）；
- 403 且响应是 HTML（Cloudflare 拦截）；
- 422（请求体不被接受，说明我们的改写和上游对不上）；
- 连接失败。

**不回落**：401（令牌问题，交给宿主刷新）、429（限流，交给宿主换号）、5xx（上游故障，照实返回）。
回落需要保留原始请求体，所以改写前把整个请求体读进内存（受 `max_body_bytes` 限制，默认 32 MiB；超限直接走 codex）。

### 2.4 响应

- 状态码、响应头、SSE 流**原样**回给宿主。宿主自己会解析 `response.completed` 做计费。
- 本阶段不改写 SSE。模型名在事件里是上游真实值（如 `gpt-5.6-sol`），没有问题。
- 风险：模型不理会提示词、仍调用 basispoints 自带工具（如 `run_officejs`）。本阶段只统计（`unexpected_tool_calls`），阶段 3 再处理。

### 2.5 配置项（加到 `internal/config`）

| 字段 | 默认 | 说明 |
|---|---|---|
| `bps_enabled` | `false` | 总开关。升级后默认不改变行为，需要管理员在配置页打开 |
| `route_mode` | `"all"` | `all` / `suffix` |
| `model_suffix` | `"-bps"` | `suffix` 模式下识别用 |
| `models` | 4 个已验证模型 | 白名单 |
| `fallback_to_codex` | `true` | 见 2.3 |
| `bps_user_agent` | 1.1 的 Edge UA | 发往 basispoints 的 UA |
| `max_body_bytes` | `33554432` | 超过就不改写 |

连接相关的旧字段保持不变。

### 2.6 状态统计（Health `status_json`，配置页显示）

`routed_bps`、`routed_codex`、按原因码的 `skip_reasons{}`、`fallbacks{}`（按原因）、
`bps_status{}`（按上游状态码计数）、`unexpected_tool_calls`（可选）。

### 2.7 TLS 指纹（风险关卡）

如果 S1 发现 Go 标准 TLS 被 Cloudflare 拦截，就换成 `github.com/refraction-networking/utls`
模拟 Chrome 的 ClientHello（sub2api 自己也依赖它，版本 `v1.8.2`）。只对 `bps.openai.com` 生效，
codex 透传不变。这会把 HTTP/2 处理复杂化，所以**先测再决定**，不预先做。

## 3. 执行清单

每步完成后在这里打勾，并在第 4 节记下结果。

- [x] **S1 连通性关卡**：写一个一次性的 Go 小程序（不进仓库），在测试服务器上用 1.1 的头和体、
      Go 标准 `net/http` 请求 basispoints，确认返回 200 还是被 Cloudflare 403。
      结论决定是否需要 2.7。（令牌从测试库读取，用完删除，不落盘、不打印。）
- [x] **S2 配置**：扩展 `internal/config`（2.5），补测试（默认值、校验、未知字段、白名单为空等）。
- [ ] **S3 路由 + 改写**：新建 `internal/basispoints`（`route.go`、`rewrite.go`、`metadata.go`），
      纯函数，全部单元测试覆盖：各原因码、后缀处理、instructions 移动、reasoning 过滤、
      item_reference 丢弃、effort 映射、metadata 确定性（同输入同输出、跨轮 task_id 不变）、头白名单。
- [ ] **S4 传输接入**：`internal/transport/forward.go` 读全请求体 → 路由 → 发 bps 或 codex →
      按 2.3 回落。`request_sent` 语义：回落前的 bps 尝试不影响最终上报（以最终那次为准，
      但只要 bps 请求头已发出，最终上报 `request_sent=true`，防止宿主重放造成重复计费）。
      测试：用 httptest 模拟 bps 返回 200/403(两种)/422/401/429/500 和连接失败，验证回落与否。
- [ ] **S5 状态 + 配置页**：Health 输出 2.6 的统计；UI 加开关、路由模式、白名单、回落开关和统计展示。
- [x] ~~**S6 如需要（取决于 S1）**：uTLS。~~ 不需要（见 S1 记录）。
- [ ] **S7 测试环境实测**：版本号改为 0.2.0，打包上传（停用 → 上传 → 启用），然后：
  1. `bps_enabled=false`：行为与 0.1.1 相同（回归）。
  2. 打开后：`/v1/responses`（流式、非流式）与 `/v1/chat/completions` 用 `gpt-5.6-sol` → 统计里
     `routed_bps` 增加，回复正常，sub2api 用量记录正常。
  3. `gpt-5.5` → 走 codex（`model_not_allowed`）。
  4. 带 `tools` 的请求 → 走 codex（`has_tools`），回复正常。
  5. `reasoning.effort=max` → 映射为 xhigh 后 200。
  6. 多轮对话（带上一轮 reasoning encrypted_content）→ 200。
  7. `suffix` 模式：试 `gpt-5.6-sol-bps`，记录宿主是否放行。
  8. 看宿主日志没有错误、插件统计里 `failed` 为 0。
- [ ] **S8 收尾**：更新 README、PLAN.md（阶段 2 打勾 + 备注）、本文第 4 节；提交并推送。

## 4. 执行记录

（每完成一步追加一条：日期、结论、遇到的问题。）

- **2026-09-25 S1 ✅**：Go 标准 `net/http`（无 uTLS）在测试服务器上请求 basispoints，
  HTTP/2 与 HTTP/1.1 **都返回 200**，模型回复 `pong`，响应 `text/event-stream; charset=utf-8`。
  之前 Python urllib 被拦是它自己的特征问题。**结论：不需要 uTLS，S6 跳过。**
  - 附带发现：账号 1 请求 basispoints 返回 **429 `usage_limit_reached`**（JSON，不是 HTML），
    说明 basispoints 的额度和账号套餐额度相关。429 按 2.3 不回落，交给宿主换号，符合预期。
  - 两个测试账号都挂了 `proxy_id=1`，所以宿主下发的 `proxy_url` 非空，插件会走账号代理。
    S1 是服务器**直连**测的，没经过这个代理；S7 实测时 bps 请求会经代理出去，要确认代理出口也能过。
- **2026-09-25 S2 ✅**：`internal/config` 增加 7 个字段（2.5），默认 `bps_enabled=false`。
  Config 里有切片，不能再用 `==` 比较，新增 `Clone()`/`Equal()`；`Pool` 存取配置都做深拷贝。
