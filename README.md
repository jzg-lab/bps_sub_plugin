# bps_sub_plugin

Sub2API 插件：把 OpenAI OAuth 账号的请求改走 OpenAI 内部 **basispoints** 端点
（`bps.openai.com/basispoints/api/responses`），以官方插件机制
（`openai.oauth.outbound_transport.v1`）交付，**不改 sub2api 源码**。

- 完整计划见 [docs/PLAN.md](docs/PLAN.md)。
- 参考：[Nonary/ghcp_proxy](https://github.com/Nonary/ghcp_proxy)、
  [Kaixxrua/excel-codex-bridge](https://github.com/Kaixxrua/excel-codex-bridge)（均 Unlicense）。

> ⚠️ 未公开的内部端点，使用可能违反 OpenAI 服务条款、导致账号封禁。风险自负。
> 推理强度上限 `xhigh`（无 `max`），且仅支持部分模型。

## 状态

**0.5.0 已上线生产**（默认不改写，按账号白名单开启）。阶段 4 起的能力：在配置页打开「启用 basispoints 改写」后，满足条件的请求改走 basispoints，
其余照旧发往 codex：

- 模型在白名单内（默认 `gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna`、`gpt-6-astra`）；不带图片或文件。
- **图片也支持**：user 消息里的内联图片先上传到 basispoints 附件端点，再用返回的 file_id 引用（带缓存、失败降级）。带远程 URL 图片、文件、音频的请求仍走 codex。
- **带工具的请求也支持**（工具中转，默认开）：basispoints 不收客户端工具，插件把工具写进提示词目录，
  模型经 `run_officejs` 发起调用，插件在 SSE 流里实时还原成 Codex 声明的 `function_call`/`custom_tool_call`，
  多轮之间用宿主 KV 回放工具调用与结果。function、custom（apply_patch）、update_plan、并行调用都支持。
  内层 envelope 兼容 `{input}` 形态（Codex Desktop/VSCode 把 apply_patch 声明成 function）、未转义引号修复；
  还原不了的调用原样交给客户端并在下一轮给出重试引导。原生模式下模型误调 basispoints 自带的 Excel/技能工具时，
  其结果被换成「改用 functions 工具」的引导。

basispoints 拒绝请求（模型无权限、请求体不兼容、被 Cloudflare 拦截、连不上）时自动回落 codex。
配置页能看到路由、回落、工具中转的计数。默认**不启用 basispoints**，升级插件不会改变现有行为。

已知限制：推理强度最高 `xhigh`（`max` 降为 `xhigh`）；文件/音频、远程 URL 图片走 codex；
「按后缀」路由模式在 sub2api 0.2.8 上不可用。
进度见 [docs/PLAN.md](docs/PLAN.md)；阶段 2/3 细节见 [docs/STAGE2.md](docs/STAGE2.md)、[docs/STAGE3.md](docs/STAGE3.md)。

## 在生产上使用（0.5.0 起）

插件已装在生产 sub2api 上。后台左侧菜单「账号管理」下面的**插件管理**（`/admin/plugins`）→ BPS Sub Plugin。

插件只对**同时满足**下面三条的请求生效，其余请求和没装插件时完全一样：

1. **进入插件**：OpenAI OAuth 账号，且落在插件页设置的「灰度比例」里（sub2api 按账号 ID 固定抽取，不能手选）。
2. **账号被选中**：配置页「只对这些账号生效」里填了这个账号 ID（留空 = 进入插件的账号全部生效）。
3. **请求符合条件**：模型在白名单内，等等（见上面「状态」）。

测试某几个账号的做法：

1. 在配置页「只对这些账号生效」填上账号 ID（每行一个），勾选「启用 basispoints 改写」，保存。
2. 这些账号必须在灰度比例内才会进入插件。灰度 10% 时进入插件的 active 账号是：
   12173、12175、12199、12228、12232、12418、12483、12492、12517、12520、12582、12614、12616、12664、12670、12707、12725、12746。
   想测别的账号，就把灰度比例调大（100% = 所有 OpenAI OAuth 账号都进入插件；不在白名单里的账号仍原样发往 codex）。
3. 看配置页运行状态：「走 basispoints」「未改写原因」「回落 codex 原因」。
4. 出问题：插件管理页点「停用」，立即恢复原生路径。

改灰度比例：先「停用」，再「启用」时填新比例。

新版 Codex（约 0.146 起）不再发顶层 `tools`，工具声明放在 `input` 的 `additional_tools` 里。basispoints 直接认识
这种声明，所以从 0.5.2 起这类请求**不走 run_officejs 中转**，工具调用和历史原样透传（状态里的「原生工具请求」）。
0.5.1 及更早会在第二轮把工具调用扣掉，表现为模型说「我先读取文件」后这一轮就结束。

从 0.5.3 起：

- **免费号默认不走 basispoints**（配置「这些套餐不走 basispoints」，默认 `free`；套餐从 access token 里读）。
  2026-09-27 实测免费号走 basispoints 会被 `403 blocked by our usage policy` 封号，business 号正常。
- basispoints 返回 usage policy 403 时，这次请求自动改走 codex，该账号 24 小时内直接走 codex（插件重启清空）。
- basispoints 自带的 `update_plan` 参数格式和 Codex 不同，插件在响应里转换、回放时转回，Codex 的计划能正常显示。

排查日志（0.6.0 起，默认开）：sub2api 把插件的 stdout/stderr 丢弃，所以插件自己写文件到
`<sub2api 数据目录>/bps-plugin-logs/`（生产即 `/opt/sub2api-deploy/data/bps-plugin-logs/`）：

- `requests-YYYYMMDD.jsonl`：每个请求一行，含 request_id、账号、模型、会话、路由与原因、状态码、首包/总耗时、
  **这一轮结局**（`tool_call` / `text` / `commentary_only` 只说要做没调工具 / `unknown_tool` 调了 Codex 没声明的工具 /
  `no_completed` 中途断开 / `failed` / `error`）。
- `bodies/YYYYMMDD/<request_id>.req.json|.resp.sse`：走 basispoints 的异常轮次和最近 200 个正常轮次的原文（不含令牌）。
- `incidents/YYYYMMDD/<时间>-<会话>/`：用户发"继续 / ？？？ / continue"这类催促时，这个会话前 5 轮 + 本轮的原文和摘要（0.6.1 起）。
- 只留 1 天、总量 ≤ 1 GB。查某个客户：先看 incidents，再按时间和模型在 jsonl 里 grep、按 request_id 打开原文。

## 构建

需要 Go 1.25+。

```bash
go test ./...                      # 单元测试
go run ./tools/packager            # 生成未签名开发包 → dist/*-unsigned.s2plugin
go run ./tools/packager -targets linux-amd64   # 只构建服务器平台，包更小
```

默认构建 `linux-amd64`、`linux-arm64`、`windows-amd64` 三个平台（见 `manifest.source.json`）。
插件 ID 和版本号在 `internal/buildinfo/buildinfo.go`，发版前改这里。

### 签名发布包

```bash
go run ./tools/packager keygen -out secrets          # 只需一次；secrets/ 已被 .gitignore
go run ./tools/packager -key secrets/publisher.key   # 生成签名包
```

把 `secrets/publisher.pub` 里的公钥配到 sub2api：

```yaml
plugins:
  trusted_publishers:
    jzg-lab-bps-v1: "<publisher.pub 的内容>"
```

私钥只留在本地，不要提交、不要放到服务器。

## 安装到 sub2api（开发期）

1. sub2api 配置里打开 `plugins.allow_unsigned: true`（只用于测试环境）。
2. 后台 → 插件管理 → 上传 `dist/*-unsigned.s2plugin`。
3. 启用插件，把 OpenAI OAuth 绑定的灰度比例设为一个小值（例如 10%）。
4. 打开插件配置页，勾选「启用 basispoints 改写」并保存。
5. 在配置页的运行状态里看「走 basispoints」「未改写原因」「回落原因」的计数。

升级插件：先停用，再上传新版本，然后重新启用。已保存的配置会保留。

## 目录

```
cmd/bps-plugin/          插件入口，只调用 pluginv1.Serve
internal/buildinfo/      插件 ID 和版本（GetInfo 与 manifest 共用）
internal/config/         配置解析、校验、默认值
internal/plugin/         TransportPlugin gRPC 服务
internal/basispoints/    路由判定、请求头/体改写、工具目录与调用还原（纯函数）
internal/tools/          工具调用映射存储（宿主 KV + 进程内 LRU 降级）
internal/transport/      Forward 流 ↔ HTTP 请求转换、basispoints 发送与回落、SSE 工具还原、连接池
internal/pluginapi/v1/   sub2api 插件契约副本（见其中 UPSTREAM.md）
ui/                      配置页（UI Bridge v1）
tools/packager/          构建、生成 manifest、签名、打包
manifest.source.json     清单中手写的部分
```
