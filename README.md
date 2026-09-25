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

计划阶段。尚无可用代码。
