package plugin

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	"github.com/jzg-lab/bps_sub_plugin/internal/tracelog"
)

// traceDirName 是自动日志目录的名字（放在 sub2api 数据目录下，和 plugins 目录平级）。
const traceDirName = "bps-plugin-logs"

// defaultTraceDir 从插件二进制路径推出日志目录。宿主把插件装在
// <data>/plugins/installed/<id>/<version>/runtimes/<os-arch>/plugin，日志放 <data>/bps-plugin-logs，
// 这样升级插件（换版本目录）不会丢日志。推不出来就放在二进制旁边。
func defaultTraceDir(executable string) string {
	dir := filepath.Dir(executable)
	for current := dir; current != filepath.Dir(current); current = filepath.Dir(current) {
		if filepath.Base(current) == "plugins" {
			return filepath.Join(filepath.Dir(current), traceDirName)
		}
	}
	return filepath.Join(dir, traceDirName)
}

// traceSettings 是影响日志 Writer 的配置，变了才重建。
type traceSettings struct {
	enabled bool
	bodies  bool
	dir     string
}

func traceSettingsOf(cfg config.Config) traceSettings {
	settings := traceSettings{enabled: cfg.TraceEnabled, bodies: cfg.TraceBodies, dir: cfg.TraceDir}
	if settings.dir == "" {
		if executable, err := os.Executable(); err == nil {
			settings.dir = defaultTraceDir(executable)
		}
	}
	return settings
}

// applyTrace 按配置打开、切换或关闭排查日志。目录建不了就不记，错误写进状态。
func (s *Server) applyTrace(cfg config.Config) {
	settings := traceSettingsOf(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.traceApplied && settings == s.traceCurrent {
		return
	}
	s.traceApplied, s.traceCurrent, s.traceError = true, settings, ""
	var writer *tracelog.Writer
	if settings.enabled && settings.dir != "" {
		created, err := tracelog.New(tracelog.Options{Dir: settings.dir, Bodies: settings.bodies})
		if err != nil {
			s.traceError = strings.TrimSpace(err.Error())
		} else {
			writer = created
		}
	}
	if old := s.forwarder.SetTrace(writer); old != nil {
		go old.Close() // 写完队列再关，不挡配置下发
	}
}

// traceStatus 是 Health 里的日志状态。
type traceStatus struct {
	Enabled   bool             `json:"enabled"`
	Dir       string           `json:"dir,omitempty"`
	Error     string           `json:"error,omitempty"`
	Written   int64            `json:"written"`
	Abnormal  int64            `json:"abnormal"`
	Incidents int64            `json:"incidents"`
	Dropped   int64            `json:"dropped"`
	Errors    int64            `json:"errors"`
	Outcomes  map[string]int64 `json:"outcomes"`
}

func (s *Server) traceStatus() traceStatus {
	s.mu.Lock()
	errText := s.traceError
	s.mu.Unlock()
	status := traceStatus{Error: errText, Outcomes: s.stats.Outcomes.Snapshot()}
	if writer := s.forwarder.Trace(); writer != nil {
		status.Enabled = true
		status.Dir = writer.Dir()
		status.Written = writer.Written.Load()
		status.Abnormal = writer.Abnormals.Load()
		status.Incidents = writer.Incidents.Load()
		status.Dropped = writer.Dropped.Load()
		status.Errors = writer.Errors.Load()
	}
	return status
}
