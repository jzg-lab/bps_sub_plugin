// Package plugin 实现 sub2api 的 TransportPlugin gRPC 服务。
package plugin

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/jzg-lab/bps_sub_plugin/internal/buildinfo"
	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
	"github.com/jzg-lab/bps_sub_plugin/internal/transport"
)

// Capability 是插件声明的唯一能力，必须与 manifest.json 一致。
const Capability = "openai.oauth.outbound_transport.v1"

// Server 实现 pluginv1.TransportPluginServer 和 pluginv1.HostBrokerReceiver。
type Server struct {
	pluginv1.UnimplementedTransportPluginServer

	pool      *transport.Pool
	stats     *transport.Stats
	forwarder *transport.Forwarder
	startedAt time.Time

	mu     sync.Mutex
	broker *hcplugin.GRPCBroker
	host   pluginv1.HostServiceClient
}

// New 创建使用默认配置的插件服务。宿主启用插件后会通过 ApplyConfig 下发已保存配置。
func New() *Server {
	pool := transport.NewPool(config.Default())
	stats := &transport.Stats{}
	return &Server{
		pool:      pool,
		stats:     stats,
		forwarder: &transport.Forwarder{Pool: pool, Stats: stats},
		startedAt: time.Now(),
	}
}

func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId:            buildinfo.PluginID,
		PluginVersion:       buildinfo.Version,
		ProtocolVersion:     pluginv1.ProtocolVersion,
		TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities:        []string{Capability},
	}, nil
}

// status 是 Health 返回的 status_json，只读取内存计数，不产生副作用。
type status struct {
	Version       string        `json:"version"`
	Mode          string        `json:"mode"`
	UptimeSeconds int64         `json:"uptime_seconds"`
	Requests      int64         `json:"requests"`
	InFlight      int64         `json:"in_flight"`
	Failed        int64         `json:"failed"`
	Cancelled     int64         `json:"cancelled"`
	HostServices  bool          `json:"host_services"`
	Config        config.Config `json:"config"`

	RoutedBPS   int64            `json:"routed_bps"`
	RoutedCodex int64            `json:"routed_codex"`
	SkipReasons map[string]int64 `json:"skip_reasons"`
	Fallbacks   map[string]int64 `json:"fallbacks"`
	BPSStatus   map[string]int64 `json:"bps_status"`
}

func (s *Server) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	s.mu.Lock()
	hostReady := s.host != nil
	s.mu.Unlock()
	cfg := s.pool.Config()
	mode := "passthrough"
	if cfg.BPSEnabled {
		mode = "basispoints_" + cfg.RouteMode
	}
	data, _ := json.Marshal(status{
		Version:       buildinfo.Version,
		Mode:          mode,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Requests:      s.stats.Total.Load(),
		InFlight:      s.stats.InFlight.Load(),
		Failed:        s.stats.Failed.Load(),
		Cancelled:     s.stats.Cancelled.Load(),
		HostServices:  hostReady,
		Config:        cfg,
		RoutedBPS:     s.stats.RoutedBPS.Load(),
		RoutedCodex:   s.stats.RoutedCodex.Load(),
		SkipReasons:   s.stats.SkipReasons.Snapshot(),
		Fallbacks:     s.stats.Fallbacks.Snapshot(),
		BPSStatus:     s.stats.BPSStatus.Snapshot(),
	})
	return &pluginv1.HealthResponse{Healthy: true, Message: "ok", StatusJson: string(data)}, nil
}

func (s *Server) ValidateConfig(_ context.Context, request *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	cfg, err := config.Parse(request.GetConfigJson())
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: err.Error()}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: cfg.JSON()}, nil
}

func (s *Server) ApplyConfig(_ context.Context, request *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, err := config.Parse(request.GetConfigJson())
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: err.Error()}, nil
	}
	s.pool.Apply(cfg)
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}

// TestConfig 只校验配置。连通性探测需要账号令牌，插件拿不到"当前账号"，
// 所以请用宿主的账号测试或真实请求验证，结果看运行状态里的统计。
func (s *Server) TestConfig(_ context.Context, request *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	started := time.Now()
	raw := request.GetConfigJson()
	if len(raw) == 0 {
		raw = s.pool.Config().JSON()
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: err.Error()}, nil
	}
	message := "配置有效；basispoints 未启用，所有请求原样透传"
	if cfg.BPSEnabled {
		message = "配置有效；basispoints 已启用（" + cfg.RouteMode + " 模式，" + strconv.Itoa(len(cfg.Models)) + " 个模型）"
	}
	health, _ := s.Health(context.Background(), nil)
	return &pluginv1.TestConfigResponse{
		Success:    true,
		Message:    message,
		LatencyMs:  time.Since(started).Milliseconds(),
		StatusJson: health.StatusJson,
	}, nil
}

func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	return s.forwarder.Forward(stream)
}

// SetHostBroker 由 pluginv1.GRPCPlugin 在注册服务时调用。
func (s *Server) SetHostBroker(broker *hcplugin.GRPCBroker) {
	s.mu.Lock()
	s.broker = broker
	s.mu.Unlock()
}

// InitHostServices 拨号回宿主，拿到 HostService 客户端（阶段 3 用它的 KV 存工具调用映射）。
func (s *Server) InitHostServices(_ context.Context, request *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	s.mu.Lock()
	broker := s.broker
	s.mu.Unlock()
	if broker == nil {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "未收到宿主 broker"}, nil
	}
	if request.GetHostServiceApiVersion() < 1 {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "宿主服务版本过低"}, nil
	}
	conn, err := broker.Dial(request.GetHostServiceId())
	if err != nil {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "连接宿主服务失败: " + err.Error()}, nil
	}
	s.mu.Lock()
	s.host = pluginv1.NewHostServiceClient(conn)
	s.mu.Unlock()
	return &pluginv1.InitHostServicesResponse{Ready: true}, nil
}
