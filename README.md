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

**阶段 1（骨架）已完成**：插件能被 sub2api 0.2.8 安装、启用，并把 OAuth 请求**原样透传**到原 codex 端点。
还没有改走 basispoints（阶段 2）。进度见 [docs/PLAN.md](docs/PLAN.md) 第 4 节。

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
4. 打开插件配置页可以看运行状态、改连接设置。

## 目录

```
cmd/bps-plugin/          插件入口，只调用 pluginv1.Serve
internal/buildinfo/      插件 ID 和版本（GetInfo 与 manifest 共用）
internal/config/         配置解析、校验、默认值
internal/plugin/         TransportPlugin gRPC 服务
internal/transport/      Forward 流 ↔ HTTP 请求转换、连接池
internal/pluginapi/v1/   sub2api 插件契约副本（见其中 UPSTREAM.md）
ui/                      配置页（UI Bridge v1）
tools/packager/          构建、生成 manifest、签名、打包
manifest.source.json     清单中手写的部分
```
