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
