# 阶段 4 执行计划：图片支持

> **状态：计划中。**
>
> 阶段 4 的**工作底稿**：先写计划，再按「执行清单」逐条做，每步勾选并在「执行记录」追加结果。
> 上下文被压缩后，从第一个没勾的步骤继续。总体路线见 [PLAN.md](PLAN.md)；前序阶段见 STAGE2/STAGE3。

## 0. 目标与范围

阶段 3 之前，带图片/文件的请求一律走 codex（原因码 `has_attachment`）。
阶段 4 让**带图片的请求也能走 basispoints**：basispoints 拒收 user 消息里的内联图片，
所以先把图片上传到 `bps.openai.com/basispoints/api/attachments`，再在请求体里用返回的
`openai_file_id` 引用。

**本阶段做**：图片（`input_image` 的 `data:` URL）上传 + 引用改写、上传失败的降级、上传缓存、
统计、配置开关、测试环境实测。

**本阶段不做**：非图片文件/音频（`input_file`/`input_audio` 继续走 codex——basispoints 未验证）；
图片的多轮缓存跨请求持久化（进程内缓存即可，重复图片重新上传代价可接受）。

## 1. 已确认的事实（2026-09-25 实测 + 读参考实现）

### 1.1 attachments 端点（实测）

- `POST https://bps.openai.com/basispoints/api/attachments`，multipart 表单，字段名 `file`，
  头和 responses 一样（去掉 `content-type`/`content-length`/`accept`，改 `accept: application/json`，
  `content-type` 用 multipart 的）。
- 成功返回 200 JSON：`{"filename":...,"content_type":...,"size":N,"openai_file_id":"file-...","input_tokens":N}`。
  取 `openai_file_id`。
- 上传后在 responses 请求体里引用：`{"type":"input_image","file_id":"file-...","detail":"auto"}`
  （去掉原来的 `image_url`）。**这个 body 形态实测返回 429（额度），不是 422（格式错）**，
  说明形态被接受；只是两个测试账号都额度受限，没能拿到 200 的视觉回复（见执行记录）。
- 账号状态说明：测试账号 1 额度用尽（`usage_limit_reached`，约 6 天后重置），账号 2 工作区停用
  （`deactivated_workspace`，402）。都是账号状态，与代码无关。

### 1.2 参考实现（excel-codex-bridge `images.py`）要点

- Codex 的图片在 input 里是 `{"type":"input_image","image_url":"data:image/png;base64,...","detail":...}`。
- 解码 data URL 拿 media_type + 字节；按 `sha256` 去重缓存（key = 账号ID + 图片哈希 → file_id）。
- 上传文件名 `picture-<hash前12>.<ext>`，ext 按 media_type（png/jpg/gif/webp）。
- 上传失败：单张降级成 `{"type":"input_text","text":"[image content omitted: ...]"}`，
  不整体失败；传输层错误（连不上）标记为"本次请求其它图片也别试了"。
- 参考实现有"先试内联、被拒再上传"的复杂逻辑，是因为 Excel 自己工具的结果图片能内联。
  **我们的场景简单**：Codex 图片都在 user 消息里，basispoints 一定拒收内联，所以
  **直接上传，不猜**。

## 2. 设计

### 2.1 路由判定（`internal/basispoints/route.go`）

- 现在：`hasAttachment` 命中就跳过（走 codex）。
- 改为：区分**图片**和**其它附件**。
  - 只有图片（`input_image` + `data:` URL）→ 进入 basispoints 路径（阶段 4 处理）。
  - 有 `input_file` / `input_audio` / 非 data 的 image_url → 仍跳过（`has_attachment`），走 codex。
- `image_support` 配置关掉时，带图片也跳过（回到阶段 3 行为）。
- `Decision` 增加 `HasImages bool`。

### 2.2 图片上传（`internal/basispoints/images.go` + `internal/transport` 接线）

上传是**网络 IO**，不能放在 basispoints 的纯函数里。分两块：

- `internal/basispoints/images.go`（纯函数）：
  - `FindImages(body)`：遍历 input，找出所有 `input_image` 的 data URL，返回 [(路径, media_type, 字节, sha256)]。
  - `ReplaceImages(body, map[sha256]result)`：把 body 里的图片换成 `{file_id}` 引用或降级文本，
    返回新 body。result 含 file_id 或 omit 原因。
  - `DecodeDataURL`、扩展名映射等辅助。
- `internal/transport`（有 IO）：
  - 上传器：`uploadAttachment(ctx, transport, header, ua, media_type, data, digest) (fileID, error)`。
    用和 bps 请求同一个 transport（同代理），复用连接。
  - 进程内上传缓存（账号ID + sha256 → file_id，LRU，带上限），避免同一图片反复上传。
  - 在 `forwardResponses` 里：路由判定为图片请求 → 先上传所有图片 → `ReplaceImages` → 再走
    原有的 BuildBody / 发送 / 工具中转流程。

### 2.3 失败降级

- 单张图片上传失败（4xx/5xx）→ 该图片替换成 `[image content omitted: <原因>]` 文本，其余照常。
- 连接失败（传输层）→ 本次请求剩余图片都降级（上游可能整个不可用）。
- 若配置 `fallback_to_codex` 且**所有**图片都上传失败 → 回落 codex（带图片走原生 codex 更稳）。
  部分成功就继续走 basispoints。
- 上传用的令牌就是请求头里的 `Authorization`（和 bps 请求同一个账号），不需要额外解析。

### 2.4 配置项

| 字段 | 默认 | 说明 |
|---|---|---|
| `image_support` | `true` | 关掉则带图片请求走 codex（阶段 3 行为）|
| `max_image_bytes` | `10485760`（10 MiB）| 单张图片解码后超过就降级为文本，不上传 |

### 2.5 统计

新增：`images_uploaded`、`images_reused`（命中缓存）、`images_omitted`（上传失败降级）、
`image_upload_errors`。

## 3. 风险

- **额度**：测试账号现在都额度受限，端到端视觉回复的 200 可能要等额度重置或换账号才能实测到。
  上传本身已验证可行，body 形态已确认被接受（429 非 422）。
- **上传延迟**：大图上传耗时，算进请求延迟。用 `max_image_bytes` 限制 + 缓存缓解。
- **多图**：一条消息多张图，逐个上传（可并行，但先串行求稳）。

## 4. 执行清单

- [x] **I1 配置**：加 `image_support`、`max_image_bytes`（校验、测试）。
- [x] **I2 图片纯函数**：`internal/basispoints/images.go`：FindImages、ReplaceImages、DecodeDataURL、
      扩展名映射；route.go 区分图片与其它附件、加 `HasImages`、`image_support` 开关。全测试覆盖。
- [x] **I3 上传器 + 缓存**：`internal/transport/attachments.go`：上传 multipart 请求、进程内 LRU 缓存、
      按 media_type 命名。用 httptest 假 attachments 服务器测（成功、4xx、连接失败、缓存命中）。
- [x] **I4 接线**：`forwardResponses` 里图片请求先上传再改写，接失败降级/回落逻辑，统计接入。
      测试：图片请求上传后走 bps、上传失败降级、全失败回落 codex。
- [x] **I5 状态 + 配置页**：Health 加图片统计；配置页加 `image_support` 开关、`max_image_bytes`、图片统计展示。
- [ ] **I6 测试环境实测**：版本 0.4.0，打包上传。测：
  1. `image_support=false`：带图片走 codex（回归阶段 3）。
  2. 打开后：带一张小图的请求 → 图片上传、body 改写、走 bps；额度允许时拿到视觉回复，
     额度不允许则确认到"上传成功 + body 被接受（非 422）+ 统计 images_uploaded 增加"。
  3. 上传失败（人为用坏账号/坏图）→ 降级为文本，不整体失败。
  4. 多轮带图 → 缓存命中（images_reused）。
  5. 宿主日志无错误，统计合理。
- [ ] **I7 收尾**：更新 README、PLAN.md、本文；提交并推送。

## 5. 执行记录

- **2026-09-25 探针**：attachments 上传实测——账号 1 上传 1x1 PNG 返回 200 `openai_file_id`；
  引用 file_id 的视觉请求返回 429（额度用尽，非 422），说明 body 形态被接受。账号 2 是
  `deactivated_workspace`（402）。上传机制确认可行，端到端视觉回复受测试账号额度限制。
- **2026-09-25 I1 ✅**：加 `image_support`（默认 true）、`max_image_bytes`（默认 10 MiB，1 KiB~64 MiB）。
- **2026-09-25 I2 ✅**：`internal/basispoints/images.go`（FindImages 去重、ReplaceImages 不改原 body、
  DecodeDataURL、ImageFileName）；route.go 用 classifyAttachments 区分内联图片和其它附件，
  只有内联图片且 image_support 开时才路由，远程图片/文件/音频仍走 codex。全测试通过。
- **2026-09-25 I3 ✅**：`internal/transport/attachments.go` multipart 上传器（连接失败标记 uploadUnavailable）
  + 进程内 LRU 缓存。测试覆盖成功/HTTP 错误/连接失败/无 file_id/缓存淘汰/URL 派生。
- **2026-09-25 I4 ✅**：forwardResponses 里图片请求先上传（超限降级、缓存命中、连接失败后续跳过），
  ReplaceImages 就地改 body，全失败且 fallback 开则回落 codex。测试：上传后走 bps 且无 data URL 外泄、
  缓存避免重传、全失败回落、image_support 关走 codex。
- **2026-09-25 I5 ✅**：状态加 images_uploaded/reused/omitted/image_upload_errors；配置页加图片开关、
  上限、图片统计展示。jsdom 验证 16 字段齐全。
