# bps_sub_plugin — 项目计划

Sub2API 插件：把 OpenAI OAuth 账号的请求改走 OpenAI 内部的 **basispoints** 端点
（`https://bps.openai.com/basispoints/api/responses`），绕开 `chatgpt.com/backend-api/codex`
的降载路由。以 sub2api 官方插件机制（`openai.oauth.outbound_transport.v1`）交付，
**不改 sub2api 源码**。

> 状态：阶段 0 已在本地准备好（等待强制推送）、阶段 1 已完成。本文随开发进度更新。

---

## 1. 背景与已验证的事实

- 目标端点 `bps.openai.com/basispoints/api/responses` 是 OpenAI 内部为 ChatGPT Excel 加载项
  提供的后端。参考实现：[Nonary/ghcp_proxy](https://github.com/Nonary/ghcp_proxy) 与
  [Kaixxrua/excel-codex-bridge](https://github.com/Kaixxrua/excel-codex-bridge)（均 Unlicense）。
- **关键验证（2026-09-25，用测试服务器上一个真实 OAuth 账号的 access_token 实测）**：
  | 结论 | 证据 |
  | --- | --- |
  | sub2api 现成的 OAuth 令牌**可直接用**，无需 Excel 令牌 | 该令牌 `aud=https://api.openai.com/v1`，按 Excel 请求头+请求体格式请求，模型正常返回 |
  | 可用模型：`gpt-5.6-sol` / `gpt-5.6-terra` / `gpt-5.6-luna` / `gpt-6-astra` | 均 HTTP 200 |
  | 不可用模型：`gpt-6-luna` / `gpt-5.5` 等 | HTTP 403 `basispoints_model_access_changed` |
  | 推理强度：`low`/`medium`/`high`/`xhigh` 可用，`max` 不可用 | `max` → HTTP 422 |
  | **客户端 `tools` 字段会被拒绝** | 带 tools → HTTP 422（这是最初集成失败的根因） |
  | `reasoning:{effort}`、`instructions`、省略 `model_selection` 均兼容 | HTTP 200 |
  | 连接特征敏感 | 服务器上 `curl` 成功，裸 `urllib` 被 Cloudflare 403；出站栈需贴近浏览器 |
- **令牌结论**：本方案（每账号一份 OAuth 令牌，sub2api 自动刷新、自动调度）优于
  Excel 桥接（每账号一台 Excel 电脑、手动推送令牌）。这是做插件相对 Excel 桥接的核心价值。

## 2. sub2api 插件机制（已确认，官方 0.2.8 具备）

- 契约：`backend/pkg/pluginapi/v1/plugin.proto`，能力 `openai.oauth.outbound_transport.v1`。
- 命中范围：仅 `platform=openai && account_type=oauth` 的**上游 HTTP 请求**。API Key、令牌刷新、
  OAuth 登录都不进插件。
- 宿主 → 插件（`Forward` 流的 `start` 帧）已包含：`method`、完整 `url`、`host`、`headers`、
  `proxy_url`、`account_id`、`platform`、`account_type`，随后是 `body_chunk`。令牌以
  `Authorization` 头形式已在请求里；也可用 HostService 的 `ResolveOutboundIdentity` 再取。
- 插件职责：建立真实上游 HTTP/TLS 连接，返回原始 `http.Response`。
- sub2api 仍负责：账号调度、令牌生命周期、SSE 解析、错误映射、用量统计、计费、下游协议。
- **灰度**：按账号 ID 稳定分桶，`rollout_percent` 可设（先 10%）。未命中的账号走原生 codex 路径。
- **失败关闭**：插件不可用时该路径直接失败，不会静默切回旧行为。
- HostService 提供：命名空间 KV（Redis 支撑，跨副本/重启共享，单值 ≤256KiB，可设 TTL）——
  用来存工具调用映射；`ListAccounts` / `ResolveOutboundIdentity`。
- 交付物：`.s2plugin`（ZIP：`manifest.json` + `signature.json` + `runtimes/<os>-<arch>/` + `ui/`）。
  Ed25519 签名；第三方公钥要加到 `plugins.trusted_publishers`；`plugins.allow_unsigned`
  仅供本地开发。

## 3. 插件必须做的三件事

1. **路由判定**：只有目标是 `/backend-api/codex/responses`、且模型在 basispoints 白名单内、
   推理强度 ≤ xhigh 时，才改写为 basispoints。其它一律**原样转发到原 codex 端点**
   （插件接管了该账号全部 OAuth 出站，含 `/responses`、`/alpha/search`、`/wham/*`、
   `/settings/*` 等，不能只处理 responses）。
2. **请求改写**：
   - URL → `https://bps.openai.com/basispoints/api/responses`，Host → `bps.openai.com`。
   - 头：去掉 codex 身份头（`originator`/`version`/`OpenAI-Beta`/`x-codex-*`），
     换成整套 `x-basispoints-auth-mode: chatgpt` + `x-openai-internal-basispoints-*` +
     `x-stainless-*` + 浏览器 `user-agent`（照 excel-codex-bridge `_DEFAULT_CLIENT_HEADERS`）。
     保留 `Authorization` / `chatgpt-account-id`，补 `x-openai-account-id`。
   - 体：`model_selection: explicit`、`store: false`、`reasoning_effort`（顶层）、
     `context_management`、`metadata`（`task_id`/`turn_id`/`agent_iteration`，从会话稳定派生）；
     **移除客户端 `tools`**，改由提示词描述（见第 3 件事）。
3. **工具中转**（工作量最大）：
   - basispoints 不收客户端工具。把 Codex 工具（shell/apply_patch…）写进提示词 catalog，
     模型经 basispoints 原生 `run_officejs` 发起调用。
   - **流式实时改写 SSE**：把 `run_officejs` 调用还原成 Codex 声明的
     `function_call` / `custom_tool_call` 事件；支持一轮多个并行调用。
   - **多轮回放**：记住"原始调用 ↔ 转换后调用"的映射（用 HostService KV，替代桥接的
     本地 sqlite），下一轮把历史工具调用/结果按上游期望的形态回放；带 `encrypted_content`
     的 reasoning 原样回放，裸 reasoning 丢弃。
   - 参考 excel-codex-bridge：`excel_upstream.py`（~1800 行）+ `excel_stream.py`（SSE 变换）+
     `sse.py`，配套测试 ~2200 行。用 Go 重写。

## 4. 里程碑

| 阶段 | 内容 | 产出 | 预估 |
| --- | --- | --- | --- |
| **0 清理** ✅ 本地完成 | 把 sub2api 的 main 恢复为纯官方 0.2.8；旧的 basispoints 开关移到 `archive/basispoints-toggle` 分支；测试环境关掉账号级开关 | sub2api 仓库干净 | 0.5 天 |
| **1 骨架** ✅ | Go 实现 `TransportPlugin`（GetInfo/Health/Validate/Apply/Test/Forward）；先做「原样透传到 codex」；打包器 + 本地未签名安装跑通 | 能安装、能转发的空插件 | 1–2 天 |
| **2 basispoints 直转（无工具）** | 路由判定 + 请求头/体改写 + 模型/效率白名单 + 出站栈贴近浏览器（TLS 指纹、连接复用）；不支持工具的请求先屏蔽或降级回 codex | 纯对话可用 | 1–2 天 |
| **3 工具中转** | catalog 注入 + SSE 实时还原 + 并行调用 + KV 多轮回放 + 图片旁路 | 带工具的 Codex 可用 | 3–5 天，需反复调 |
| **4 配置页 + 验收** | UI Bridge 配置页（模型白名单、效率映射、工具开关、灰度）；单测 + 宿主集成测试；测试服灰度实测 | 可发布的签名包 | 1–2 天 |

## 5. 目录结构（阶段 1 建立）

```
bps_sub_plugin/
├── cmd/bps-plugin/main.go        # 仅调用 pluginv1.Serve
├── internal/buildinfo/          # 插件 ID + 版本（GetInfo 与 manifest 共用）
├── internal/config/             # 配置解析/校验/默认值
├── internal/plugin/             # TransportPlugin gRPC 服务
├── internal/transport/          # 路由判定 + 请求改写 + 上游连接
├── internal/basispoints/        # 头/体改写、模型白名单、effort 映射（阶段 2）
├── internal/tools/              # 工具 catalog + SSE 还原 + KV 回放（阶段 3）
├── internal/pluginapi/v1/       # sub2api 插件契约副本
├── ui/index.html                # 配置页（sandbox iframe + UI Bridge）
├── tools/packager/              # 打包 + 计算 SHA-256 + 生成 manifest.json + 签名
├── manifest.source.json
├── docs/PLAN.md                 # 本文
└── README.md
```

## 6. 已知风险与红线

- **封号风险最高的用法**：把 Excel 专用后端接进多用户中转。上线务必**先小比例灰度**，
  监控 403/429/账号异常，再逐步放量。参考项目作者明确不建议中转/转售。
- **上游易变**：内部端点随时可能改字段、加校验、换模型清单。白名单与错误处理要可配置、可回滚。
- **无 max、模型受限**：basispoints 顶到 xhigh；只支持第 1 节那 4 个模型。其余请求必须回落 codex，
  不能报错给用户。
- **"防降智"无实证**：该后端面向 Excel 场景会注入自有提示词，编程效果需实测，不预设更强。
- **依赖 sub2api 版本**：`requires.sub2api` 锁定 0.2.x；宿主升级要重测 `transport_api` 兼容性。
- **密钥**：Ed25519 发布私钥只在本地/离线，绝不进仓库或服务器；`trusted_publishers` 只配公钥。

## 7. 决定记录

- [x] 阶段 0：sub2api main 恢复为纯官方 0.2.8（仅保留 README 顶部 fork 说明），旧开关归档到
      `archive/basispoints-toggle`。强制推送由你执行。（2026-09-25 确认）
- [x] 签名：开发期用 `allow_unsigned` 未签名包；发布期用本项目自建 Ed25519 密钥
      （`go run ./tools/packager keygen`），公钥加到 `plugins.trusted_publishers`。（2026-09-25 确认）
- [x] Go module：`github.com/jzg-lab/bps_sub_plugin`；插件 ID：`io.github.jzg-lab.bps-sub-plugin`。
- [ ] 上线节奏：建议先灰度 10% 再放量（阶段 4 前确认）。

## 8. 阶段 1 实现备注

- sub2api 的 Go module 在仓库 `backend/` 子目录，不能直接 `go get`，所以把契约文件
  复制到 `internal/pluginapi/v1/`（见其中 `UPSTREAM.md`）。升级宿主时整体覆盖。
- 透传语义：请求头原样转发（去掉 `Content-Length`/`Transfer-Encoding`/`Host`/`Connection`，
  由 net/http 重新生成）；`Host` 用宿主给的值；不自动加 `Accept-Encoding`、不自动解压。
- `request_sent`：用 `httptrace.WroteHeaders` 判定。请求头还没写出就失败（连不上、代理错误）
  报 `false`，宿主可以换账号重试；其余情况一律 `true`。
- 连接池按代理地址复用 `http.Transport`，最多 256 个，超出淘汰最久未用的；改配置时整体换新。
- 已知差异：宿主原生路径对 codex 端点可能使用 TLS 指纹（uTLS），插件目前用 Go 标准 TLS。
  阶段 2 需要评估 basispoints 是否对 TLS 指纹敏感（第 1 节"连接特征敏感"）。
- 已验证：用 sub2api 0.2.8 自己的安装器、`startPluginRuntime`、`roundTrip` 跑通了
  安装 → 启动 → 配置 → 转发 → 错误帧（在 sub2api 仓库里临时加测试，跑完删除）。
