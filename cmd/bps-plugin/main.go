package main

import (
	"github.com/jzg-lab/bps_sub_plugin/internal/plugin"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

func main() {
	pluginv1.Serve(plugin.New())
}
