# 阶段 5 执行计划：账号白名单 + 上线生产

> 阶段 5 的**工作底稿**：先写计划，再按「执行清单」逐条做，每步勾选并在「执行记录」追加结果。
> 上下文被压缩后，从第一个没勾的步骤继续。总体路线见 [PLAN.md](PLAN.md)。

## 0. 目标

1. 用户要**自己指定**哪些 OpenAI OAuth 账号走 basispoints。
2. 把插件装到生产 sub2api（`/opt/sub2api-deploy`，端口 8080）。用户已于 2026-09-27 同意「现在就装」。

## 1. 已确认的事实（2026-09-27，只读检查）

- 生产镜像 tag 是 `sub2api:rollback-v0.2.6-49a39b6`，但二进制是 **Sub2API 0.2.8**
  （commit fd80b08c），带插件系统；`/api/v1/admin/plugins` 路由存在。
- 设置 `plugin_management_enabled=true`：后台左侧菜单「账号管理」下面有插件管理，路径 `/admin/plugins`。
- `sub2api_plugin_installations` / `sub2api_plugin_bindings` 均 0 行（没装过插件）。
- `data/config.yaml`（挂载到 `/app/data`）没有 `plugins` 段：`allow_unsigned=false`，没有可信发布者，
  所以必须用签名包，并在 config.yaml 加 `plugins.trusted_publishers`，**改完要重启 sub2api 容器**（只读启动时配置）。
- 宿主灰度（sub2api 0.2.8 `plugin_manager.go`）：`rollout_percent` 1~100，按
  `stablePluginBucket(account.ID) < rollout` 选账号（murmur 风格哈希 % 100），**不能手选账号**。
  命中的账号，WebSocket 入口会被强制改为 HTTP Bridge。
- 插件 start 帧里有 `account_id`（sub2api 账号 ID）。
- 生产：openai/oauth active 130 个、error 70 个。

## 2. 设计：账号白名单

- 配置新增 `account_ids []int64`（sub2api 账号 ID）。**空 = 不限制**（兼容旧行为）；
  非空时只有列表里的账号才改走 basispoints，其它账号原样发往 codex，统计原因码 `account_not_selected`。
- 判定放在 `forward.go` 进入 `forwardResponses` 之前（不读 body，零开销），账号不在列表时直接透传。
- 校验：每个 ID > 0，最多 1000 个，去重。
- 配置页：「只对这些账号生效」文本框，每行或逗号分隔一个账号 ID。
- 版本 0.5.0。
- 注意：要让用户任意选账号，宿主 `rollout_percent` 必须设 100（否则账号可能根本不进插件）。
  100% 意味着所有 OpenAI OAuth 请求都经过插件进程（非白名单账号原样透传，行为与不装插件一致，
  但插件进程挂了会影响全部这类请求——宿主会报「插件不可用」而不是自动绕过）。

## 3. 上线步骤（生产）

1. 本地 `keygen` 生成 `secrets/publisher.key` / `.pub`（已 gitignore，不入库、不上服务器）。
2. 签名打包 linux-amd64。
3. 备份 `data/config.yaml` → `data/config.yaml.bak.<日期>-before-plugin`；追加
   `plugins.trusted_publishers.jzg-lab-bps-v1: <pub>`。
4. `docker restart sub2api`，确认 healthy、首页和 API 正常。
5. 用管理员 API 上传签名包 → 先保存配置（`bps_enabled=true`、`account_ids=[]`→ 见下）→ 启用。
   **启用前**先把 `account_ids` 设成一个用户还没选时也安全的值：`bps_enabled=false`（纯透传），
   让用户在配置页填好账号后自己勾选启用。
6. 启用绑定。实际先用 `rollout_percent=10`（28 个 OAuth 账号，其中 18 个 active），用户确认后再调。
7. 冒烟：状态页 healthy、`routed_codex` 增加、失败 0、sub2api 日志无插件错误。
8. 回退方案：后台「停用」插件即恢复原生路径；最坏情况恢复 config.yaml 备份并重启。

## 4. 执行清单

- [x] **P1 白名单代码**：config 字段 + 校验 + 测试；forward 判定 + 测试；状态原因码。
- [x] **P2 配置页**：account_ids 输入框、原因码中文；版本 0.5.0。
- [x] **P3 测试环境验证**：上传 0.5.0，白名单内/外账号各跑一次看路由统计。
- [x] **P4 签名包**：keygen、签名打包，测试环境用签名流程走一遍（可选）。
- [x] **P5 生产配置 + 重启**：备份、加 trusted_publishers、重启、健康检查。
- [x] **P6 生产安装**：上传、配置（bps 关）、启用 100%、冒烟。
- [x] **P7 收尾**：README 写生产使用说明；PLAN/本文更新；提交推送。

## 5. 执行记录

- **2026-09-27 调研**：见第 1 节。
- **2026-09-27 P1 ✅**：`config.AccountIDs`（去重、>0、≤1000、默认 `[]`）+ `AccountSelected`；
  forward.go 在进入 forwardResponses 前判定，不在列表 → 原因码 `account_not_selected`、原样透传。
  测试：配置解析/去重/非法值、白名单内走 bps、白名单外走 codex 且 body 不变。
- **2026-09-27 P2 ✅**：配置页「只对这些账号生效」文本框（换行/逗号/中文逗号分隔，前端校验纯数字）；
  补原因码中文；版本 0.5.0。jsdom 验证加载、保存、非法输入拦截。
- **2026-09-27 P3 △**：测试环境升级 0.5.0 成功（runtime_healthy=true）；宿主保存 `account_ids`、
  拒绝 0/-1。**请求级分流未能在测试环境实测**：账号 1 的 codex 7 天额度 100%（10-01 重置），
  账号 2 error，宿主 `no available OpenAI accounts (pool=0)`，请求到不了插件。分流逻辑由单测覆盖，
  上线生产后用白名单账号实测。
- **2026-09-27 P4 ✅**：`keygen` 生成密钥（key id `jzg-lab-bps-v1`，公钥 `D7BuasCjB4S85Fp+27BHo2gLzR6G16nSK9wBdywz4+U=`）；
  私钥在本地 `secrets/`（gitignore）并备份到 `~/.config/bps_plugin_publisher.key`（600），不上服务器。
  测试环境在 `/app/data/config.yaml` 追加 `plugins.trusted_publishers` 后重启，签名包上传 →
  `signature_status=trusted`、启用 healthy。（环境变量设 map 不可靠，统一用 config.yaml。）
- **2026-09-27 P5 ✅**：生产 `data/config.yaml` 备份为 `data/config.yaml.bak.2026-09-27-before-plugin`，
  追加 `plugins.trusted_publishers.jzg-lab-bps-v1`；`docker restart sub2api`，11 秒 healthy，首页 200，流量恢复。
- **2026-09-27 P6 ✅**：签名包放在 `/opt/sub2api-deploy/plugin-packages/`（sha256 `e02ee1c0…`），
  用 admin API key（`x-api-key`，step_up 关闭）上传 → id 1、`signature_status=trusted`、兼容 0.2.8。
  先存配置 `bps_enabled=false`、`account_ids=[]`（纯透传），再启用 **rollout 10%**。
  3 分钟观察：插件 healthy，37 个请求全部原样发往 codex，failed 0；这些账号的错误只有上游
  `503 overloaded`（已恢复，原生账号同样有），与插件无关。
  **basispoints 尚未打开**，由用户在配置页填账号、勾选启用。
- **2026-09-27 P7 ✅**：README 写生产使用说明；提交推送。
- **2026-09-27 0.5.1 修复：配置页保存没反应**。宿主用 `<iframe sandbox="allow-scripts">`（没有 `allow-forms`）
  加载配置页，浏览器直接丢弃表单提交，Chromium 报 "Blocked form submission … 'allow-forms' permission is not set"，
  `submit` 事件都不触发（阶段 2~4 用 jsdom 验证，没有 sandbox，所以没发现）。改成保存按钮 `type=button` + click 处理，
  并拦截回车提交。验证：headless Chromium 在 sandbox iframe 里复现旧版 0 次保存、新版保存成功；测试环境真实后台
  （0.5.1 签名包）点保存 →「已保存」，服务端 `account_ids` 已写入。包放在本地 `release/0.5.1/`，由用户上传到生产。

## 6. 0.5.2 修复：新版 Codex 用着用着中断

### 6.1 现象与根因（2026-09-27 实测）

客户反馈：模型说「我先读取文件」然后这一轮就结束了，没有执行任何工具。

- 新版 Codex（本机 0.148，生产上 0.146~0.158 都有）**不再发顶层 `tools`**，工具放在 `input` 里的
  `{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[exec, wait, …]}, …]}`。
  抓包：本机 codex exec 指向本地假服务器，请求体没有 `tools` 键。
- 插件 `ParseTools` 只看顶层 `tools` → 目录为空 → 当成无工具对话：原样把 `additional_tools` 透传，
  再加一句 `ExternalClientInstructions`（"Return the answer as assistant text"）。
- **basispoints 本身认识 `additional_tools`**：用生产账号直发插件生成的请求体，模型返回原生
  `custom_tool_call name=exec`（input 是 `tools.exec_command(...)` 的 JS，正是 Codex 声明的工具）。
- 第一轮走无工具路径（`relay` 直接透传），调用能到 Codex；第二轮起历史里有 `custom_tool_call`，
  `hasToolHistory` 命中 → 走 `relayToolStream`：它**扣留**所有原生工具事件，completed 时用（空的）catalog
  还原失败 → 工具调用被丢掉，只剩 commentary「我先读取…」→ Codex 认为这一轮结束。这就是「中断」。
  用抓到的真实 basispoints SSE 跑当前变换确认：输出里只剩 reasoning + completed。
  （顺带：历史回放把 `exec` 调用 fallback 包成 `run_officejs`，basispoints 也照样接受并继续调用 exec。）
- 把 basispoints 的原生 SSE（`custom_tool_call name=exec`，无 namespace）原样喂给本机 Codex CLI：
  Codex 直接执行 exec、下一轮原样回放 `custom_tool_call` + `custom_tool_call_output`，basispoints 也接受
  （生产账号实测第二轮 200，继续调用 exec）。**所以 `additional_tools` 请求应当整条原样透传，不做中转。**
- 另：`403 This request was blocked by our usage policy` 在同一账号上对任何请求（含最简单的 pong）都返回，
  是账号级封禁，与请求内容无关；目前插件不回落，直接把 403 给宿主（宿主按 403 计数、标 error）。

### 6.2 设计

- `Decide`：input 里有 `additional_tools` 项 → `Decision.NativeTools=true`，**不走工具中转**
  （`HasToolContext=false`），响应用 `relay` 原样透传。
- `BuildBody`：`NativeTools` 时不加 `ExternalClientInstructions`（它叫模型只回文本，和工具冲突）；
  `translateInput` 对 `function_call`/`custom_tool_call` 历史原样保留，不回放成 run_officejs。
  `additional_tools` 项原样保留（上游认识）。
- 状态统计加 `native_tools`（走原生工具路径的请求数），配置页显示「原生工具请求」。
- `tool_relay` 关闭时 additional_tools 请求也走 codex（原因码 `has_tools`），和顶层 tools 一致。
- usage policy 403：不改（回落到 codex 同一账号大概率也被拦；让宿主正常处理账号状态）。

### 6.3 执行清单

- [x] **N1 代码**：route.go / rewrite.go / 统计 + 配置页；单测（用抓到的 Codex 请求结构）。
- [x] **N2 端到端**：本机 Codex CLI → 本地假上游（回放真实 basispoints SSE）经插件变换，确认工具调用到达 Codex；
      生产账号直发新 BuildBody 生成的两轮请求体，确认 200 且返回原生工具调用。
- [x] **N3 打包**：版本 0.5.2，签名包放 `release/0.5.2/`，由用户上传。
- [x] **N4 收尾**：README/本文记录；提交推送。

### 6.4 执行记录

- **2026-09-27 调研**：见 6.1。
- **2026-09-27 N1 ✅**：`Decision.NativeTools`（input 有非空 additional_tools 且没有顶层 tools）；
  `BuildBody` 此时不注入 catalog/`ExternalClientInstructions`，`translateInput` 原样保留工具调用和结果；
  响应走 `relay` 原样透传；统计 `native_tools` + 配置页。单测：原生透传、tool_relay 开关、空 additional_tools；
  transport 测试 `TestAdditionalToolsCallReachesHost`（旧代码下失败：工具调用被扣留，只剩 created/completed）。
- **2026-09-27 N2 ✅**：
  - 本机 Codex CLI 0.148 → 本地 harness（插件 Forwarder）→ 假上游回放两段真实 basispoints SSE：
    **新代码**：exec 执行 2 次后收到 DONE，3 轮请求；**旧代码**：exec 执行 1 次后停在
    「目录里有一个 TeX 主文件…我继续展开读取。」，只有 2 轮请求——正是客户截图里的现象。
  - 生产账号直发新代码对 Codex 真实第二轮请求生成的请求体：12789 / 12536 / 12783 都 200，
    模型读到工具结果给出最终答案。12432、12609 对任何请求（含 pong）都 403 usage policy，是账号级封禁。
- **2026-09-27 N3 ✅**：0.5.2 签名包 sha256 `21d36f48…`，放在 `release/0.5.2/`；测试环境签名包升级 → healthy。
- **2026-09-27 N4 ✅**：README、本文；提交推送。由用户上传生产（停用 → 上传 → 启用，配置保留）。

## 7. 0.5.3：免费号被封 + update_plan 格式

### 7.1 事实（2026-09-27 实测）

- 客户反馈"又不干活"发生在 21:16 之前（0.5.2 或更早都在跑）。
- **免费号被封**：今天 40 个账号收到 `403 This request was blocked by our usage policy`（约 100 次，19~21 点），
  **全是 free 套餐**；14 个已被宿主标 error。business（`self_serve_business_prolite`）号 12783/12789/12536 一直正常。
  封禁是账号级（被封号发最简单的 pong 也 403），但被封号走 codex 仍正常（12596/12432 codex luna 200）。
  插件目前对这个 403 不回落 → 客户请求失败，宿主连续 3 次 403 就把账号停掉。
- 套餐类型：宿主给插件的账号信息去掉了 credentials，拿不到 plan_type；但请求头里的 access token（JWT）
  payload 的 `https://api.openai.com/auth.chatgpt_plan_type` 就是套餐（6 个账号实测与库里一致，库里空的 12432 JWT 里是 free）。
- **update_plan**：原生工具模式下，basispoints 模型会调用它自带的 `update_plan`，参数是
  `{"summary":…,"plan":[{"id","description","status","result"}]}`；Codex 要 `{"explanation":…,"plan":[{"step","status"}]}`，
  直接报 `unknown field summary`（把真实 basispoints 返回喂给 Codex CLI 复现），计划不显示、浪费一轮。

### 7.2 设计

- **按套餐跳过**：配置 `exclude_plan_types`（默认 `["free"]`）。Decide 从 Authorization 的 JWT 读套餐，
  命中 → 原因码 `plan_excluded`，原样走 codex。读不出套餐（非 JWT、没有该字段）不拦。
- **usage policy 403 回落 + 冷却**：basispoints 返回 403 且正文含 `blocked by our usage policy` →
  回落 codex（原因码 `usage_policy`，受 `fallback_to_codex` 控制），并把这个 chatgpt 账号记进内存冷却表 24 小时，
  期间该账号直接走 codex（原因码 `policy_cooldown`）。重启插件清空。状态显示冷却中的账号数。
- **update_plan 双向转换**（仅原生工具模式）：
  - 响应：SSE 里 `name=update_plan` 的 function_call，扣住它的 arguments.delta，在 arguments.done 时
    发一条转换后的 delta + done；output_item.done、response.completed 里的参数一并改写。其余事件原样透传。
  - 历史：Codex 回放的 update_plan 调用转回原生参数、结果换成 `{"status":"ok"}`（沿用中转模式的已有逻辑）。

### 7.3 执行清单

- [x] **U1 套餐跳过**：JWT 解析 + config + Decide + 配置页；单测。
- [x] **U2 usage policy 回落 + 冷却**：fallbackReason、冷却表、状态；单测。
- [x] **U3 update_plan 转换**：响应 SSE 改写 + 历史回放；单测；真实 basispoints 返回喂 Codex CLI 验证计划正常显示；
      真实 basispoints 验证回放后的第二轮 200。
- [x] **U4 打包收尾**：0.5.3 签名包放 `release/0.5.3/`、测试环境装一遍；README/本文；提交推送。

### 7.4 执行记录

- **2026-09-27 调研**：见 7.1。
- **2026-09-27 U1 ✅**：`basispoints.PlanType` 解 JWT payload 读 `chatgpt_plan_type`；config `exclude_plan_types`
  （默认 `["free"]`，小写去重，≤32 个）；Decide 在身份检查后判定，命中原因码 `plan_excluded`；配置页文本框。
  单测：JWT 解析（大小写、非 JWT、坏 base64）、free 跳过 / business 放行 / 空列表放行、配置规范化。
- **2026-09-27 U2 ✅**：403 正文含 `blocked by our usage policy` → 回落原因 `usage_policy`，该 chatgpt 账号
  进 `Stats.PolicyCooldown`（内存，24 小时）；冷却期内原因码 `policy_cooldown` 直接发原始请求到 codex；
  状态 `policy_cooldown_accounts` + 配置页。单测：回落、冷却期不打 basispoints、到期恢复、关回落时原样返回 403。
- **2026-09-27 U3 ✅**：`basispoints.ClientPlanArguments`（原生 → Codex：summary→explanation、description→step、
  状态别名归一，未知状态当 pending；已是 Codex 形态原样返回）。原生工具模式响应改走 `relayNativeStream`：
  其它事件逐字透传，update_plan 的 arguments.delta 扣住、done 时补一条完整 delta，item.done / completed 里的参数一并改写。
  回放：原生模式下 update_plan 调用转回原生格式（沿用 `restoreNativeFunctionArguments`）、结果换 `{"status":"ok"}`，
  其它原生调用仍原样。中转模式的 update_plan 还原方向也修正为原生 → 客户端（之前反了，旧单测按错误方向写的，已改）。
  验证：本机 Codex CLI → 插件 Forwarder → 真实 basispoints（账号 12783），任务「先 update_plan 再统计 Python 文件行数」：
  5 轮全 200，Codex 正常显示计划并逐项打勾（→ / ✓），无 `unknown field summary`；回放给 basispoints 的是原生格式 + `{"status":"ok"}`。
- **2026-09-27 U4 ✅**：配置页对旧配置（无该字段）显示默认 free，避免一保存就清空（headless Chromium 验证：显示 free、
  保存带 `["free"]`、状态显示原生工具请求/冷却账号数、原因码中文）。0.5.3 签名包 sha256 `4a6f604e…`，
  放在 `release/0.5.3/`；测试环境升级 → healthy。README/本文；提交推送。由用户上传生产。


## 8. 与原生 Codex 的差异对照（2026-09-27，对照两个参考项目 + 实测）

参考项目（本地 `/tmp/refs`，只读）：`Kaixxrua/excel-codex-bridge`（8a277df）、`Nonary/ghcp_proxy`（dfb758b）。
两者都**只走 run_officejs 中转**（从不把工具发给 basispoints，也不认识 `additional_tools`），
唯一的原生↔客户端工具映射是 `update_plan`。basispoints 自带工具（实测回显 25 个）：update_plan、
request_user_input_basispoints、read_ranges/write_range 等 Excel 工具、list_skills/read_skills/create_skill/update_skill、
list_connectors/run_connector_action、run_officejs、web_search。

### 8.1 已实测的差异（原生工具模式，新版 Codex）

| basispoints 行为 | Codex 结果 | 实测 |
|---|---|---|
| 自带 `update_plan`（summary/description） | 0.5.3 已转换 | 正常 |
| `request_user_input_basispoints`（多一个 summary） | `unsupported call`；改名映射后 Codex exec 模式报 "unavailable in Default mode"（TUI 才可用） | 5 个任务里 2 次调用 |
| Excel/技能工具（list_skills、read_sheets_metadata…） | `unsupported call`，浪费一轮 | 提到 Excel 时就调 |
| 服务端 web_search | 服务端搜完直接回答 | 正常 |
| exec（含 view_image、apply_patch） | 正常 | 正常 |
| 工具结果里的内联图片 | 422 → 回落 codex；换成 file_id 后 basispoints 接受（200） | |

### 8.2 参考项目有、插件没有（或不同）

| 项 | excel-codex-bridge | ghcp_proxy | 插件 |
|---|---|---|---|
| 心跳：静默 15 秒发 `response.in_progress` | 有（仅带工具时） | 无 | 无（设计写了未实现） |
| reasoning 显示规范化（`**Thinking**` 前缀、summary/content） | 有（仅转换了工具调用时） | 同 | 无 |
| 工具结果 `unsupported call: run_officejs` → 重试指引 | 有 | 有 | 无（中转模式） |
| 提示词写明 "list_skills、web-search 等不可用" | 有 | 有 | 中转模式有（少 list_skills）；原生模式 0.5.2 起无 |
| 中转模式回落重建时 namespace 丢失 | 有此问题 | 有此问题 | 同 |
| astra 只接受 medium/high/xhigh | 无 | 有（low→medium） | 无；实测 astra low 也 200，无需改 |
| 工具结果里的图片上传 | 被拒后按类型逐级上传/省略 | 仅 user 消息 | 已扫描整个 input 上传（含工具结果） |
| 其余（字段白名单、metadata、reasoning 回放、item_reference、passthrough 元数据、update_plan 双向、结果 `{"status":"ok"}`、空输出补文案、反斜杠修复、双层嵌套、断流补 completed、图片上传缓存、身份头） | 有 | 有 | 已有 |
