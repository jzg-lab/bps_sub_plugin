// Package buildinfo 是插件身份的唯一来源。GetInfo 和打包器生成的 manifest.json
// 都从这里取值，宿主启动时会校验两者一致。
package buildinfo

const (
	// PluginID 是插件在宿主中的唯一标识，发布后不要修改。
	PluginID = "io.github.jzg-lab.bps-sub-plugin"
	// Version 是插件语义化版本。每次发布前递增。
	Version = "0.4.0"
)
