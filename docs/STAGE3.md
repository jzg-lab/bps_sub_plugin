# 阶段 3 执行计划：工具中转

> **状态：计划中。**
>
> 阶段 3 的**工作底稿**：先写计划，再按「执行清单」逐条做，每步勾选并在「执行记录」追加结果。
> 上下文被压缩后，从第一个没勾的步骤继续。总体路线见 [PLAN.md](PLAN.md)，阶段 2 见 [STAGE2.md](STAGE2.md)。

## 0. 目标与范围

阶段 2 把**不带工具**的对话请求改走了 basispoints；带工具的请求还在走 codex。
阶段 3 让**带工具的请求也能走 basispoints**——这是 Codex 编程场景的主体。

basispoints 不接受客户端声明的 `tools`（带了就 422）。做法（沿用 excel-codex-bridge，已实测可行）：

1. 把客户端工具写进**提示词目录（catalog）**，请求体里不带 `tools`。
2. 模型通过 basispoints 自带的 `run_officejs` 函数发起调用，`code` 字段里是内层 JSON
   `{"name":"<工具名>","arguments":{...}}`（custom 工具用 `"input":"..."`）。
3. 插件在 **SSE 流里实时**把 `run_officejs` 调用还原成客户端声明的 `function_call` / `custom_tool_call`。
4. 下一轮：把历史里的工具调用/结果**回放**成上游认识的形态（原生 `run_officejs` 调用 + 结果）。

**本阶段做**：catalog 注入、SSE 实时还原、并行调用、多轮回放（用 HostService KV 存映射）、`update_plan` 特例。

**本阶段不做**：图片上传（带图片仍走 codex，阶段 4 或以后）；非流式带工具（先只支持 `stream:true`，
不满足就走 codex——Codex CLI 默认就是流式）。

## 1. 已确认的事实（2026-09-25 实测 + 读参考实现/宿主源码）

### 1.1 basispoints 工具协议（实测）

用 catalog 提示词 + `run_officejs` 实测（测试账号 2，模型 gpt-5.6-sol）：

- 模型先发一条 `phase:commentary` 的 message（"我来看下当前目录"），再发一个
  `type:function_call`、`name:run_officejs` 的调用。**commentary 在工具调用之前**——
  参考实现据此判断"流是否被中途截断"。
- `run_officejs` 的 `arguments`（字符串化 JSON）形如：
  ```json
  {"summary":"...","extended_summary":"...","code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}","destructive":false,"references":["_shell"]}
  ```
  内层工具在 `code` 里，是**字符串化的 JSON**（不是 JS）。
- 调用项有 `call_id`（如 `call_Qq...`）和 `id`（如 `fc_09...`）。
- SSE 事件序列：`response.created` → `in_progress` → `output_item.added`(message) →
  `content_part.added` → 多个 `output_text.delta` → `output_text.done` → `content_part.done` →
  `output_item.done`(message) → `output_item.added`(function_call) →
  多个 `function_call_arguments.delta` → `function_call_arguments.done` →
  `output_item.done`(function_call) → `response.completed`。
- 第二轮：把 catalog+reminder+user、原生 function_call、`function_call_output`（`call_id` 对应）
  一起回放，模型正确读到工具结果并给出最终答案（`/home/tester/project`）。
- reasoning：turn1 没返回 reasoning 条目（effort=low）；带 `encrypted_content` 的按阶段 2 的规则回放。

### 1.2 客户端（Codex）工具形态（宿主交给插件的请求体）

- `tools` 是数组，元素 `{"type":"function","name":...,"description":...,"parameters":{JSON Schema}}`
  或 `{"type":"custom","name":...,"format":{...}}`；还可能有 `{"type":"namespace","name":...,"tools":[...]}`
  嵌套（动态工具）。Codex 常见工具：`shell`/`exec_command`（function）、`apply_patch`（custom）、
  `update_plan`（function）、`read_file` 等。
- `tool_choice`：`"none"` 表示这轮不给工具（此时按无工具处理，正常走 basispoints）；`"auto"` 常见。
- `parallel_tool_calls`：默认视为允许（Responses API 默认 true）。
- 历史里的工具调用/结果：`function_call` / `function_call_output` / `custom_tool_call` /
  `custom_tool_call_output`，用 `call_id` 关联。
- **宿主已对保留工具名做过改写**（`aliasOpenAIOAuthReservedToolNames`，如把 `python` 改名），
  插件拿到的已是改写后的名字，插件按拿到的名字原样处理即可，不用管这层。

### 1.3 HostService KV（宿主源码 `plugin_host_services.go`）

- `KVGet`/`KVSet`/`KVDelete`/`KVList`，命名空间隔离，Redis 支撑，跨副本共享。
- 限制：key ≤256 字节，value ≤256 KiB，TTL 上限 90 天，`namespace`/`key` 仅
  `[A-Za-z0-9._-]`，List 有条数上限。
- 阶段 1 已经在 `InitHostServices` 里拿到了 `HostServiceClient`（`Server.host`）。

## 2. 设计

### 2.1 路由判定的变化（`internal/basispoints/route.go`）

阶段 2 里"带工具"和"有工具历史"是**跳过**原因。阶段 3 改为：

- 有 `tools`（且 `tool_choice != "none"`）→ **进入工具中转路径**（不再算 skip）。
- 有工具历史（`function_call` 等）→ 也进入工具中转路径（要回放）。
- 仍然跳过（走 codex）：带图片/文件（`has_attachment`）、**非流式**（`stream:false` 且带工具，
  因为 SSE 还原只在流式做）、请求体过大。
- 无工具、无工具历史 → 阶段 2 的纯对话路径不变。

`Decision` 增加：`Tools`（解析出的 catalog 规格）、`ToolChoiceNone`、`HasToolContext`（有工具或工具历史）。

### 2.2 catalog 注入（`internal/basispoints/tools.go`）

- 从 `tools` 解析出工具目录（含 namespace 展开），生成两条 developer 消息：
  **catalog 说明**（协议 + 工具 JSON 目录）和 **reminder**（精简重复提醒），都放在 input 最前面
  （instructions 之后、历史之前），保持前缀稳定利于上游缓存。文案照搬参考实现
  `_client_tool_protocol_instructions` / `_client_tool_protocol_reminder`（实测有效）。
- 请求体**不带 `tools`**；`tool_choice`、`parallel_tool_calls` 也不带。
- 无工具时退回阶段 2 的单条 `ExternalClientInstructions`。

### 2.3 历史回放（`internal/basispoints/tools.go`，`translateInput` 扩展）

阶段 2 的 `translateInput` 目前丢掉工具历史。阶段 3 改为：

- `function_call` / `custom_tool_call`：查 KV 里记住的**原生调用**（键 = call_id），有就原样回放
  （保留上游 item 身份，encrypted reasoning 才认）；没有（插件重启/换副本且 KV 没命中）就用
  `_fallback_transport_call` 重建一个 `run_officejs` 调用。记录 `call_id → 上游工具名` 的来源映射。
- `function_call_output` / `custom_tool_call_output`：按 `_normalized_tool_output` 处理——
  `update_plan` 的结果换成 `{"status":"ok"}`；空输出补 `(tool call succeeded with no output)`；
  custom 输出在经 run_officejs 时改成 `function_call_output`；对齐 `id` = `fc_<call_id>`。
- `update_plan` 是 basispoints 原生工具，走原生 function_call（`_restore_native_function_arguments`
  把 Codex schema 转回上游 schema）。

### 2.4 SSE 实时还原（`internal/transport/toolstream.go`）

对带工具的流式响应，在 `relay` 和上游之间插一层变换（照搬 `excel_stream.py` 逻辑）：

- 逐条解析 SSE 事件（`event:` + `data:`）。
- **扣留**原生工具事件（`function_call_arguments.delta/done`、`custom_tool_call_input.*`、
  以及 `output_item.added/done` 里 type 为 function_call/custom_tool_call 的），不直接发给宿主。
- 到 `response.completed` 时，从 output 里提取 `run_officejs` 调用，用 catalog 规格校验并还原成
  客户端声明的 `function_call`/`custom_tool_call`（`_client_call_from_native` + schema 校验），
  把每个还原后的调用**重新合成**成标准 SSE 事件序列（added → arguments.delta → arguments.done →
  item.done）发给宿主，再发改写后的 `response.completed`。同时把原生调用存进 KV（供下一轮回放）。
- 不是工具调用的 message、reasoning 正常透传（reasoning 按 `normalize_reasoning_item_for_client`
  让 Codex 能显示）。
- 边界情况（照搬参考实现）：上游在最后一个完成项之后断流 → 用已完成项合成 `response.completed`；
  上游长时间静默 → 周期性重发 `response.in_progress` 防止 Codex 5 分钟超时；旧式文本 marker
  （`<codex_tool_call>`）也能解析（兼容）。

### 2.5 KV 映射（`internal/tools/store.go` 或并入 basispoints）

- 命名空间：固定串（如 `toolcalls`）。key = call_id（满足 `[A-Za-z0-9._-]`；不满足就 sha256）。
- value = 原生 `run_officejs` 调用项的 JSON（≤256 KiB；超了就存精简版/跳过）。
- TTL：设一个合理值（如 7 天），避免无限增长。
- 宿主没提供 HostService（`Server.host==nil`）时降级：用进程内 LRU（重启丢失，
  表现为回放走 fallback 重建，功能不塌）。
- 读写要能容错：KV 出错只记数、不影响转发。

### 2.6 配置项（`internal/config`）

| 字段 | 默认 | 说明 |
|---|---|---|
| `tool_relay` | `true` | 工具中转总开关；关掉则带工具的请求继续走 codex（回到阶段 2 行为）|
| `tool_call_ttl_seconds` | `604800`（7 天）| KV 里工具调用映射的 TTL |

`bps_enabled` 仍是总开关。`tool_relay` 只在 `bps_enabled` 时有意义。

### 2.7 统计（Health `status_json`）

新增：`tool_relayed`（还原成客户端工具的调用数）、`tool_replayed`（回放的历史调用数）、
`tool_fallback_rebuilt`（KV 没命中重建的次数）、`tool_decode_failed`（内层 JSON 解不出）、
`kv_errors`。路由计数里带工具的请求计入 `routed_bps`。

## 3. 风险

- **最大风险：模型不稳定**。可能不用 run_officejs 直接执行、把内层 JSON 写坏、嵌套 run_officejs。
  参考实现用大量提示词约束 + JSON 修复 + 重试引导应对。我们照搬，但**必须实测**验证效果（S6）。
- **SSE 还原的正确性**：事件顺序、output_index、item id 要让 Codex 认。靠单测（合成上游流）+ 实测。
- **多轮 + encrypted reasoning**：turn1 的 reasoning 要按 store=false 规则回放，工具调用要能对上。
- **KV 一致性**：多副本共享 Redis，但测试环境是单副本；单副本下进程内缓存也够，KV 主要为重启/多副本。
- **回退保证**：`tool_relay` 关闭、或任何解析失败，都要能干净地走 codex，不能让带工具请求变差。

## 4. 执行清单

- [x] **T1 配置**：加 `tool_relay`、`tool_call_ttl_seconds`（含校验、测试）。
- [x] **T2 catalog + 工具解析**：`internal/basispoints/tools.go`：解析 tools（含 namespace）、
      生成 catalog/reminder developer 消息、内层 envelope 解码（含 JSON 反斜杠修复）、schema 校验、
      原生调用 → 客户端调用还原、fallback 重建、update_plan 双向转换。纯函数，全测试覆盖。
- [ ] **T3 路由 + 请求体**：route.go 把"带工具/有工具历史"从 skip 改为进入中转；BuildBody 注入
      catalog、扩展 translateInput 做历史回放；非流式带工具仍走 codex。测试。
- [ ] **T4 SSE 还原**：`internal/transport/toolstream.go`：SSE 解析器 + 扣留 + completed 时还原 +
      重新合成事件 + keepalive + 断流补完。用合成的上游 SSE 流做单测（含并行调用、非工具、断流）。
- [ ] **T5 KV 映射 + 接线**：KV 存取（宿主 KV，降级进程内 LRU）；forwardResponses 里带工具的
      basispoints 响应经 toolstream 变换；回放时查 KV。统计接入。
- [ ] **T6 测试环境实测**：版本 0.3.0，打包上传（停用→上传→启用）。用真实 Codex 风格请求测：
  1. `tool_relay=false`：带工具走 codex（回归到阶段 2 行为）。
  2. 打开后：带 `exec_command`(shell) 的请求 → 走 bps，模型发起调用，插件还原成 function_call，
     回放结果后模型给出正确答案（pwd / ls 之类）。
  3. `apply_patch`（custom 工具）→ 还原成 custom_tool_call。
  4. `update_plan` → 原生处理，结果回放正常。
  5. 并行调用（一次多个工具）→ 都还原。
  6. 多轮（调用→结果→再调用）→ KV 回放，encrypted reasoning 不报错。
  7. 用真实 Codex CLI 指向测试网关跑一个小任务（读文件、改文件），看能不能跑通。
  8. 宿主日志无错误，插件统计合理，失败数低。
- [ ] **T7 收尾**：更新 README、PLAN.md（阶段 3 打勾）、本文；提交并推送。

## 5. 执行记录

（每完成一步追加：日期、结论、遇到的问题。）

## 5. 执行记录

- **2026-09-25 探针**：用 catalog 提示词 + run_officejs 在测试账号 2 上实测：模型先发 commentary message，
  再发 `function_call name=run_officejs`，`code` 里是字符串化的 `{"name":"exec_command","arguments":{"cmd":"pwd"}}`；
  第二轮回放原生 function_call + function_call_output 后，模型正确读到工具结果。协议可行，照搬参考实现。
- **2026-09-25 T1 ✅**：加 `tool_relay`（默认 true）、`tool_call_ttl_seconds`（默认 7 天，60 秒~90 天）。
- **2026-09-25 T2 ✅**：`internal/basispoints/tools.go`（目录解析、catalog/reminder 生成）、`relay.go`
  （原生调用 → 客户端调用还原、run_officejs 解包含双层嵌套、JSON 反斜杠修复、直呼工具名兼容）、
  `schema.go`（轻量 JSON Schema 校验、update_plan 双向转换）。全测试通过。
