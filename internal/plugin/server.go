// Package plugin 实现 sub2api 的 TransportPlugin gRPC 服务。
package plugin

import (
	"context"
	"encoding/json"
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
}

func (s *Server) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	s.mu.Lock()
	hostReady := s.host != nil
	s.mu.Unlock()
	data, _ := json.Marshal(status{
		Version:       buildinfo.Version,
		Mode:          "passthrough",
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Requests:      s.stats.Total.Load(),
		InFlight:      s.stats.InFlight.Load(),
		Failed:        s.stats.Failed.Load(),
		Cancelled:     s.stats.Cancelled.Load(),
		HostServices:  hostReady,
		Config:        s.pool.Config(),
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

// TestConfig 在阶段 1 只做配置校验；阶段 2 起会增加上游连通性探测。
func (s *Server) TestConfig(_ context.Context, request *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	started := time.Now()
	raw := request.GetConfigJson()
	if len(raw) == 0 {
		raw = s.pool.Config().JSON()
	}
	if _, err := config.Parse(raw); err != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: err.Error()}, nil
	}
	health, _ := s.Health(context.Background(), nil)
	return &pluginv1.TestConfigResponse{
		Success:    true,
		Message:    "配置有效，当前为原样透传模式",
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
