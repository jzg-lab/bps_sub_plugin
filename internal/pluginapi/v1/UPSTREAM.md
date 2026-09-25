# 上游契约副本

本目录是 sub2api 插件契约的**原样副本**，不要手改：

- 来源：[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) `backend/pkg/pluginapi/v1/`
- 版本：v0.2.8（commit `a3eb7ef30`）
- 许可：LGPL-3.0（随上游）

sub2api 后端的 Go module 路径是 `github.com/Wei-Shaw/sub2api`，但代码位于仓库的
`backend/` 子目录，无法直接 `go get`，因此复制到这里。

升级宿主版本时，用新版本的这 5 个文件整体覆盖，并更新上面的版本和 commit：

```
plugin.proto  plugin.pb.go  plugin_grpc.pb.go  runtime.go  manifest.schema.json
```
