# BPS 限流与 403 被动排查

相关核对：[2026-09-29 生产异常计数](ERROR_TRIAGE_20260929.md)。该快照来自生产
0.6.4，区分回落、HTTP 错误、工具协议问题和取消；不能当作 0.6.5 新字段的验证结果。

## 目标与边界

2026-09-29（Asia/Shanghai）用户确认：先增强日志，保留一天，供后续观察分析。
只采集，不增加上游请求，不改路由、回落、重试、并发、模型或超时；不部署到生产。

待区分：1000 RPM 的计数范围；模型 TPM 是否跨工作区共享；普通 JSON 403、usage policy、
模型权限和 HTML 拦截的区别；宿主重试及附件请求是否放大流量；回落对缓存命中的影响。
观察相关性不等于证明限流规则。插件灰度外、其他实例和其他客户端的流量不一定可见。

## 实施清单

- [x] 增加每次上游尝试的开始/结束摘要，保留成功、失败、附件和回落各条路径。
- [x] 增加稳定脱敏身份、独立会话/缓存键、发送时间、限流头和原始 SSE 终态/用量。
- [x] 验证透明转发、取消、截断、重复错误去重、隐私边界和一天清理。
- [x] 更新版本与使用文档，测试、签名打包；交付通过本次 Git 提交保存。生产部署另行确认。

## 已确认的旧日志不足

- `chatgpt_account` 只有前八位；`session` 优先使用缓存键，不能严格代表会话。
- `time` 是请求结束后异步写日志的时间，不能直接当作请求发出时间。
- 只记录最终转发结果，不能完整保留回落前、附件上传等每次 HTTP 尝试。
- 没有记录限流响应头；HTTP 200 可能包含 `error`/`response.failed`。

## 安全与分析约定

新增记录不包含请求/响应正文、令牌、Cookie、代理密码或明文身份。身份使用带类型域的
SHA-256 截断摘要进行关联；这是去标识化，不是可对外公开的匿名数据。JWT 只读取必要
身份字段，标明未经此日志功能验证，不参与授权或路由。代理配置标识不是出口 IP。

保持现有对话原文策略，不扩大原文留存。新增摘要使用同一异步写入和容量清理机制，
记录丢弃计数；日志不完整时不能把未观察到的请求视为不存在。

## 0.6.5 文件与字段

保持原有 `trace_enabled` 开关，不新增配置，`trace_bodies=false` 不影响摘要采集。

- `requests-YYYYMMDD-HH.jsonl`：保留旧的最终转发摘要字段，新增 `forward_id`、
  `instance_id`、`started_at`、`finished_at`、`identity`。旧 `session`、
  `chatgpt_account` 为兼容保留，不把它们当真实身份的充分依据。
- `upstream-YYYYMMDD-HH.jsonl`：`schema_version=1`，每个实际 RoundTrip 两行，
  分别为 `upstream_start` 和 `upstream_finish`；以 `(instance_id, forward_id, attempt)`
  连接。一次转发中的附件、BPS、原生 Codex 回落分别计数；附件缓存命中不伪造上传事件。
- 只有开始没有结束：可能还在进行、进程退出、切配置或日志丢失，不擅自判定超时。

| 问题 | 保存的证据 | 不能直接推出的结论 |
| --- | --- | --- |
| 是否按工作区/用户限制 | Sub 账号 ID；完整工作区 ID 的稳定 hash；JWT workspace/user/subject hash；header/JWT 工作区是否一致 | 不把多个 Sub ID 当多个独立工作区；JWT 字段未在日志模块验签 |
| 是否按会话或缓存键限制 | header session、body session、conversation/thread、prompt_cache_key 分开 hash；client request hash（存在才有） | 缓存键不等于会话，客户端字段不一定被上游用于限流 |
| 是否按出口共享 | 代理 endpoint、代理用户名关联 hash；direct/account_proxy；host hash、进程 instance | 代理配置不是实际出口，NAT/轮换代理/同出口其他进程不可见；`exit_ip_known=false` |
| RPM、并发还是耗时 | started_at；WroteHeaders/WroteRequest 时间和次数；首个响应字节、响应头、结束时间；本地 inflight；宿主下发的账号并发配置 | inflight 包含建连/连接池等待和响应体生命周期，不是上游实时并发；并发上限不等于固定 RPM |
| 403/429 是什么类型 | 原始 HTTP 状态、Content-Type、白名单响应头；JSON/SSE error code/type、消息 hash 和规则归类 | generic JSON 403 仍为 forbidden_unknown；HTML 403 不能单凭格式断言是某一家 WAF |
| 1000 RPM 或模型 TPM | 明确错误模板中的 metric、limit、used、requested、window_seconds、model、organization hash | 无提示的作用域不猜；上游 org 不自动等于 ChatGPT workspace；正则未识别不代表没有限流 |
| 是否为 HTTP 200 流内失败 | 改写前 SSE 的 error、response.failed/incomplete/completed；嵌入式 status 与 HTTP 状态分开记 | HTTP 200 不是任务成功；终态之后的客户端取消不是新的上游失败 |
| 回落是否影响缓存 | 分端点的实际 input/output/cached/reasoning tokens；缓存键、会话 hash | 缺少 usage 是未知，不是 0；缓存统计不能证明两条路共用物理缓存 |

`endpoint` 只写 `bps_responses`、`bps_attachments`、`codex_responses`、`codex_other`
或 `other`，不记录原始 URL/query。纯透传请求不额外缓冲请求体，所以模型/缓存键可能未知。
`request_content_length=-1` 表示长度未知，不以零代替。`headers_written_at` 等回调缺失
表示未观测到；存在多次写头时记录次数和最后一次时间，不能把它们精确拆成多次完整请求。

响应头白名单：`x-request-id` / `request-id`、`cf-ray` / `cf-cache-status` / `server`、
`date`、`retry-after` / `retry-after-ms`、`content-type`（去掉参数）、
`openai-processing-ms`、请求/Token 的 `x-ratelimit-limit/remaining/reset-*`、
Codex primary/secondary 的用量百分比、窗口分钟和 reset-after-seconds。值需通过
长度和类型验证。响应 `OpenAI-Organization` / `OpenAI-Project` 只保存 hash。
不保存任意头，不保存 Authorization、Cookie、Set-Cookie 或原始传输异常文本。

## 成本、保留与完整性

- **保留策略仍为 24 小时，容量目标 5 GiB**，每 10 分钟清理一次。摘要改成小时分片，
  按文件最后修改时间整片清理：最早的行最多约多留 1 小时 + 10 分钟，不再因日分片多留近一天。
  老版本已有的日文件按原修改时间自然过期；停机/关闭日志期间不运行清理，下次启动会扫描。
  容量也是定期检查，检查间隔内可能短暂超限；超容量优先删原文，摘要最后删。
  流量足够大时，容量清理可能使实际可回溯窗口不足一天；24 小时是保留策略，不是完整采集保证。
- 正常请求也完整留元数据，不抽样。每次上游尝试增加两条摘要，仍使用非阻塞队列 1024。
  队列满或 writer 已关闭时丢弃并计数，不让磁盘压力阻塞转发。
- Health 的 `trace.diagnostic_written` / `trace.diagnostic_dropped` 分别是新增摘要写入/
  丢弃数，`dropped` 包含所有日志丢弃，`errors` 包含写盘错误；配置页未新增展示控件。
  每条新增事件有 `diagnostic_dropped_before_enqueue`。进程/Writer 重启计数重置，
  应同时看 instance 变化、开始/结束配对和宿主退出日志。写盘错误与队列丢弃不是一回事。
- 原始响应只旁观已经读到的字节，不主动读、读尽或重发。单事件解析上限 256 KiB；
  超大 SSE 事件跳过后继续看后续事件。单条响应最多保存 8 个不同错误，重复错误合并。
  `oversized_events`、`invalid_events`、`error_signals_dropped` 暴露采集不完整。
- 对话正文策略不变：旧 bodies/incidents 仍可能含用户敏感内容，不应公开上传。
  新增元数据本身也可关联身份，hash 不等于匿名。未记录的字段/错误模板不能还原。
- 不增加探活、重试、回落、限流器或真实上游实验；普通 JSON 403、429 的原有转发行为不变。

## 下一轮分析顺序

1. 先检查版本、instance、Writer 丢弃/写盘错误和 start/finish 配对率。收集同期宿主
   request_id、调度/冷却原因和最终状态日志；插件无法直接观察宿主之后合成的 503。
2. 用 **发送时间**做滚动 60 秒请求计数，并分别看 1 秒/10 秒突发；不要用结束时间代替
   发送时间，也不要只看自然分钟桶。分别计算 BPS Responses、附件、原生 Codex。
   没有 wire 发送回调的尝试单列，不冒充已发出的请求。
3. 对 Sub 账号、workspace、user、header/body session、proxy、model 以及必要的组合
   分组，看错误是否同步开始和恢复、失败前请求数与 inflight、成功对照是否存在。
   检查是否多个账号其实共享同一 workspace 或 user，避免虚假的跨账号结论。
4. 分开 generic 403、policy/model access、HTML 拦截、RPM、TPM、额度用尽和传输失败。
   TPM 优先看明确 org + model + limit 的错误，不把成功 usage 合计当作上游 TPM 定义。
5. 用同一缓存键/会话下、相近模型与输入规模的成功样本比较 BPS 和 Codex 的
   cached/input；失败请求的缺失用量不能按零计算。
6. 只输出证据强弱和待验证假设。没有足够自然对照时，再单独确认小流量、单变量实验，
   不靠轮换账号或扩大请求量来试探限制。未知的其他客户端流量始终是混杂变量。

## 验证与交付

- 新增单测覆盖脱敏、会话分离、JWT 不匹配、403/429、RPM/TPM、流内失败、
  SSE 分片/CRLF/多行、超大事件后的错误、零缓存与未知值、非流式 usage、错误去重/上限、
  附件缓存命中、回落尝试、原样字节/错误、httptrace 组合、取消、inflight 回收和 Writer 并发关闭。
- `go test ./...`、`go test -race ./...`、`go vet ./...`；本机通过 WSL Ubuntu-22.04
  的 Go 1.27.0 运行。打包目标 linux-amd64，仍使用已有发布密钥，不输出或上传私钥。
- 本轮没有升级、停用、重启生产，也没有向真实 BPS 发送测试请求。部署确认后新字段才开始采集。
- Obsidian CLI 尝试返回“无法找到运行中的 Obsidian”；本轮长期分析框架先保留在本文，未绕过 CLI 写入 vault。

最终签名包：`release/0.6.5/io.github.jzg-lab.bps-sub-plugin-0.6.5.s2plugin`，
linux-amd64，5,900,481 字节；已独立验证 manifest 版本、全部文件 SHA-256 和 Ed25519 签名。
包 SHA-256：`3cec3ba63d7a3de712ac9e0d7fe8d5329644999d2e4b508bfe0b3a0a9d622689`。
Git HTTPS 直连失败后，使用机器已有系统代理进行命令级同步，未改全局 Git 配置。
