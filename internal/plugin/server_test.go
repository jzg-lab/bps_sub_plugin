package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jzg-lab/bps_sub_plugin/internal/buildinfo"
	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

func TestGetInfoMatchesBuildInfo(t *testing.T) {
	info, err := New().GetInfo(context.Background(), &pluginv1.GetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.PluginId != buildinfo.PluginID || info.PluginVersion != buildinfo.Version {
		t.Fatalf("identity = %s@%s", info.PluginId, info.PluginVersion)
	}
	if info.ProtocolVersion != pluginv1.ProtocolVersion || info.TransportApiVersion != pluginv1.TransportAPIVersion {
		t.Fatalf("protocol = %d/%d", info.ProtocolVersion, info.TransportApiVersion)
	}
	if len(info.Capabilities) != 1 || info.Capabilities[0] != Capability {
		t.Fatalf("capabilities = %v", info.Capabilities)
	}
}

func TestHealthReportsStatusJSON(t *testing.T) {
	health, err := New().Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil || !health.Healthy {
		t.Fatalf("health = %+v, %v", health, err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(health.StatusJson), &snapshot); err != nil {
		t.Fatalf("status_json is not JSON: %v", err)
	}
	if snapshot["mode"] != "passthrough" || snapshot["version"] != buildinfo.Version {
		t.Fatalf("status_json = %v", snapshot)
	}
}

func TestValidateConfigNormalizes(t *testing.T) {
	server := New()
	response, err := server.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: []byte(`{}`)})
	if err != nil || !response.Valid {
		t.Fatalf("validate = %+v, %v", response, err)
	}
	if string(response.NormalizedConfigJson) != string(config.Default().JSON()) {
		t.Fatalf("normalized = %s", response.NormalizedConfigJson)
	}
	response, err = server.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: []byte(`{"bogus":true}`)})
	if err != nil || response.Valid || response.Message == "" {
		t.Fatalf("invalid config accepted: %+v, %v", response, err)
	}
}

func TestApplyConfigIsAtomicAndRepeatable(t *testing.T) {
	server := New()
	good := []byte(`{"max_idle_conns_per_host":3}`)
	for i := 0; i < 2; i++ {
		applied, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: good})
		if err != nil || !applied.Applied {
			t.Fatalf("apply #%d = %+v, %v", i, applied, err)
		}
	}
	rejected, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{"max_idle_conns_per_host":0}`)})
	if err != nil || rejected.Applied {
		t.Fatalf("invalid apply = %+v, %v", rejected, err)
	}
	if got := server.pool.Config().MaxIdleConnsPerHost; got != 3 {
		t.Fatalf("failed apply must keep previous config, got %d", got)
	}
}

func TestTestConfigUsesAppliedConfigWhenEmpty(t *testing.T) {
	result, err := New().TestConfig(context.Background(), &pluginv1.TestConfigRequest{})
	if err != nil || !result.Success || result.StatusJson == "" {
		t.Fatalf("test = %+v, %v", result, err)
	}
	result, err = New().TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: []byte(`[]`)})
	if err != nil || result.Success {
		t.Fatalf("invalid test config = %+v, %v", result, err)
	}
}

func TestInitHostServicesWithoutBroker(t *testing.T) {
	response, err := New().InitHostServices(context.Background(), &pluginv1.InitHostServicesRequest{HostServiceId: 1, HostServiceApiVersion: 2})
	if err != nil || response.Ready {
		t.Fatalf("init without broker = %+v, %v", response, err)
	}
}
