// Package tracelog 把每个请求的摘要和异常轮次的原文写到磁盘，供排查"用着用着不动了"这类问题。
// 宿主把插件的 stdout/stderr 丢弃（io.Discard），这是插件唯一能留下现场的地方。
// 写盘在后台 goroutine 里做，任何错误只计数，绝不影响转发。
package tracelog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultRetention 是日志保留时长：只是排错用，留一天就够。
	DefaultRetention = 24 * time.Hour
	// DefaultMaxBytes 是日志目录总量上限，超了先从最旧的原文删起，摘要（requests-*.jsonl）最后才删。
	DefaultMaxBytes = 5 << 30
	// recentNormal 是保留原文的最近正常轮次数（超过的正常轮次原文会被删掉）。
	recentNormal = 200
	// maxBodyBytes 是单个原文文件的上限，超出部分截断。
	maxBodyBytes = 8 << 20

	queueSize     = 1024
	cleanupPeriod = 10 * time.Minute

	// 催促事件（用户发"继续"）时一起保存的上下文：每个会话在内存里留最近 historyTurns 轮原文。
	historyTurns    = 5
	historySessions = 200
	historyIdle     = 30 * time.Minute
)

// Entry is a forward summary in an hourly requests-YYYYMMDD-HH.jsonl file.
type Entry struct {
	ForwardID      string    `json:"forward_id,omitempty"`
	Instance       string    `json:"instance_id,omitempty"`
	StartedAt      string    `json:"started_at,omitempty"`
	FinishedAt     string    `json:"finished_at,omitempty"`
	Identity       *Identity `json:"identity,omitempty"`
	Time           string    `json:"time"`
	RequestID      string    `json:"request_id"`
	AccountID      int64     `json:"account_id,omitempty"`
	ChatGPTAccount string    `json:"chatgpt_account,omitempty"`
	Plan           string    `json:"plan,omitempty"`
	Model          string    `json:"model,omitempty"`
	Session        string    `json:"session,omitempty"`
	Route          string    `json:"route"`            // bps / codex
	Reason         string    `json:"reason,omitempty"` // 没走 bps 的原因或回落原因
	NativeTools    bool      `json:"native_tools,omitempty"`
	Status         int       `json:"status,omitempty"`
	FirstByteMs    int64     `json:"first_byte_ms,omitempty"`
	DurationMs     int64     `json:"duration_ms"`
	BytesOut       int64     `json:"bytes_out,omitempty"`
	Outcome        string    `json:"outcome"`
	Tools          []string  `json:"tools,omitempty"`
	UnknownTools   []string  `json:"unknown_tools,omitempty"`
	Text           string    `json:"text,omitempty"` // 这一轮最后一段助手文字（截断）
	UpstreamError  string    `json:"upstream_error,omitempty"`
	Error          string    `json:"error,omitempty"`
	Nudge          bool      `json:"nudge,omitempty"`     // 本轮用户消息是"继续 / ？？？"之类的催促
	Incident       string    `json:"incident,omitempty"`  // 催促时保存的上下文目录（相对日志目录）
	Cancelled      bool      `json:"cancelled,omitempty"` // 宿主提前结束（客户端断开、宿主读到 completed 后关流、插件停用）
	Truncated      bool      `json:"truncated,omitempty"` // 响应太大，只保存了前一部分
	Bodies         bool      `json:"bodies,omitempty"`    // 是否保存了原文
}

// Record 是交给 Writer 的一条记录：摘要加可选原文。
type Record struct {
	Diagnostic *UpstreamEvent
	Entry
	Request  []byte // 发往上游的请求体（不含请求头，没有令牌）
	Response []byte // 返回给宿主的响应体
	Abnormal bool   // 异常轮次一定保存原文
}

// Writer 异步写日志。零值不可用，用 New 创建；nil *Writer 的方法都是空操作。
type Writer struct {
	dir       string
	bodies    bool
	retention time.Duration
	maxBytes  int64
	now       func() time.Time

	queue     chan Record
	done      chan struct{}
	wg        sync.WaitGroup
	enqueueMu sync.RWMutex
	closed    bool

	mu     sync.Mutex
	normal []string // 最近正常轮次的原文文件（旧的在前）

	// history 按会话留最近几轮（只在写线程里访问，不用加锁）。
	history map[string]*sessionHistory

	Written           atomic.Int64
	Dropped           atomic.Int64
	Errors            atomic.Int64
	Abnormals         atomic.Int64
	Incidents         atomic.Int64
	DiagnosticWritten atomic.Int64
	DiagnosticDropped atomic.Int64
}

// sessionHistory 是一个会话最近几轮的原文。
type sessionHistory struct {
	turns []Record
	last  time.Time
}

// Options 是 Writer 的配置。
type Options struct {
	Dir       string
	Bodies    bool
	Retention time.Duration
	MaxBytes  int64
}

// New 创建目录并启动后台写线程。目录建不了返回错误（调用方可以不记日志继续跑）。
func New(options Options) (*Writer, error) {
	if options.Retention <= 0 {
		options.Retention = DefaultRetention
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(filepath.Join(options.Dir, "bodies"), 0o700); err != nil {
		return nil, err
	}
	w := &Writer{
		dir:       options.Dir,
		bodies:    options.Bodies,
		retention: options.Retention,
		maxBytes:  options.MaxBytes,
		now:       time.Now,
		queue:     make(chan Record, queueSize),
		done:      make(chan struct{}),
		history:   map[string]*sessionHistory{},
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

// Dir 返回日志目录。
func (w *Writer) Dir() string {
	if w == nil {
		return ""
	}
	return w.dir
}

// Log 把记录放进队列；队列满了就丢弃并计数，绝不阻塞转发。
func (w *Writer) Log(record Record) {
	if w == nil {
		return
	}
	w.enqueueMu.RLock()
	defer w.enqueueMu.RUnlock()
	if w.closed {
		w.drop(record)
		return
	}
	if record.Abnormal || record.Nudge {
		w.Abnormals.Add(1)
	}
	select {
	case w.queue <- record:
	default:
		w.drop(record)
	}
}

func (w *Writer) drop(record Record) {
	w.Dropped.Add(1)
	if record.Diagnostic != nil {
		w.DiagnosticDropped.Add(1)
	}
}

// LogDiagnostic takes ownership of event maps and slices; callers must not mutate them.
func (w *Writer) LogDiagnostic(event UpstreamEvent) {
	if w == nil {
		return
	}
	event.WriterDropped = w.DiagnosticDropped.Load()
	w.Log(Record{Diagnostic: &event})
}

// Close 写完队列里的记录后停止。
func (w *Writer) Close() {
	if w == nil {
		return
	}
	w.enqueueMu.Lock()
	if !w.closed {
		w.closed = true
		close(w.done)
	}
	w.enqueueMu.Unlock()
	w.wg.Wait()
}

func (w *Writer) loop() {
	defer w.wg.Done()
	w.cleanup()
	ticker := time.NewTicker(cleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case record := <-w.queue:
			w.write(record)
		case <-ticker.C:
			w.cleanup()
			w.expireHistory()
		case <-w.done:
			for {
				select {
				case record := <-w.queue:
					w.write(record)
				default:
					return
				}
			}
		}
	}
}

func (w *Writer) write(record Record) {
	now := w.now()
	if record.Diagnostic != nil {
		line, err := json.Marshal(record.Diagnostic)
		if err != nil {
			w.Errors.Add(1)
			return
		}
		file, err := os.OpenFile(filepath.Join(w.dir, "upstream-"+now.Format("20060102-15")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			w.Errors.Add(1)
			return
		}
		_, err = file.Write(append(line, 10))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			w.Errors.Add(1)
			return
		}
		w.DiagnosticWritten.Add(1)
		return
	}
	day := now.Format("20060102")
	entry := record.Entry
	if entry.Time == "" {
		entry.Time = now.Format(time.RFC3339Nano)
	}

	if w.bodies && record.Nudge && entry.Session != "" {
		if dir, ok := w.writeIncident(record, now); ok {
			entry.Incident = dir
		}
	}
	if w.bodies && entry.Session != "" {
		w.remember(record, now)
	}

	if w.bodies && (len(record.Request) > 0 || len(record.Response) > 0) {
		dir := filepath.Join(w.dir, "bodies", day)
		base := filepath.Join(dir, safeName(entry.RequestID, now))
		if err := os.MkdirAll(dir, 0o700); err == nil {
			ok := writeCapped(base+".req.json", record.Request) && writeCapped(base+".resp.sse", record.Response)
			if ok {
				entry.Bodies = true
				if !record.Abnormal {
					w.rememberNormal(base)
				}
			} else {
				w.Errors.Add(1)
			}
		} else {
			w.Errors.Add(1)
		}
	}

	line, err := json.Marshal(entry)
	if err != nil {
		w.Errors.Add(1)
		return
	}
	file, err := os.OpenFile(filepath.Join(w.dir, "requests-"+now.Format("20060102-15")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		w.Errors.Add(1)
		return
	}
	_, err = file.Write(append(line, '\n'))
	_ = file.Close()
	if err != nil {
		w.Errors.Add(1)
		return
	}
	w.Written.Add(1)
}

// remember 把这一轮放进会话历史（只留最近 historyTurns 轮）。
func (w *Writer) remember(record Record, now time.Time) {
	history := w.history[record.Session]
	if history == nil {
		if len(w.history) >= historySessions {
			w.evictOldestSession()
		}
		history = &sessionHistory{}
		w.history[record.Session] = history
	}
	record.Request = capBody(record.Request)
	record.Response = capBody(record.Response)
	history.turns = append(history.turns, record)
	if len(history.turns) > historyTurns {
		history.turns = append([]Record(nil), history.turns[len(history.turns)-historyTurns:]...)
	}
	history.last = now
}

func (w *Writer) evictOldestSession() {
	var oldest string
	var oldestAt time.Time
	for session, history := range w.history {
		if oldest == "" || history.last.Before(oldestAt) {
			oldest, oldestAt = session, history.last
		}
	}
	delete(w.history, oldest)
}

func (w *Writer) expireHistory() {
	cutoff := w.now().Add(-historyIdle)
	for session, history := range w.history {
		if history.last.Before(cutoff) {
			delete(w.history, session)
		}
	}
}

// writeIncident 把会话最近几轮和本轮一起写到 incidents/YYYYMMDD/<时间>-<会话>/，返回相对目录。
// 文件按顺序编号：01-<request_id>.req.json … 最后一个是本轮（催促的这一轮）。
func (w *Writer) writeIncident(record Record, now time.Time) (string, bool) {
	session := safeName(record.Session, now)
	if len(session) > 8 {
		session = session[:8]
	}
	relative := filepath.Join("incidents", now.Format("20060102"), now.Format("150405")+"-"+session)
	dir := filepath.Join(w.dir, relative)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.Errors.Add(1)
		return "", false
	}
	turns := append([]Record(nil), w.history[record.Session].turnsOrNil()...)
	turns = append(turns, record)
	var summary []byte
	for i, turn := range turns {
		prefix := filepath.Join(dir, fmt.Sprintf("%02d-%s", i+1, safeName(turn.RequestID, now)))
		if len(turn.Request) > 0 && !writeCapped(prefix+".req.json", turn.Request) {
			w.Errors.Add(1)
		}
		if len(turn.Response) > 0 && !writeCapped(prefix+".resp.sse", turn.Response) {
			w.Errors.Add(1)
		}
		line, _ := json.Marshal(turn.Entry)
		summary = append(append(summary, line...), '\n')
	}
	if os.WriteFile(filepath.Join(dir, "summary.jsonl"), summary, 0o600) != nil {
		w.Errors.Add(1)
	}
	w.Incidents.Add(1)
	return relative, true
}

func (h *sessionHistory) turnsOrNil() []Record {
	if h == nil {
		return nil
	}
	return h.turns
}

func capBody(data []byte) []byte {
	if len(data) > maxBodyBytes {
		return data[:maxBodyBytes]
	}
	return data
}

// rememberNormal 记住正常轮次的原文，只保留最近 recentNormal 个。
func (w *Writer) rememberNormal(base string) {
	w.mu.Lock()
	w.normal = append(w.normal, base)
	var evict []string
	if len(w.normal) > recentNormal {
		evict = append(evict, w.normal[:len(w.normal)-recentNormal]...)
		w.normal = append([]string(nil), w.normal[len(w.normal)-recentNormal:]...)
	}
	w.mu.Unlock()
	for _, old := range evict {
		_ = os.Remove(old + ".req.json")
		_ = os.Remove(old + ".resp.sse")
	}
}

// cleanup 删掉过期文件，并在总量超限时从最旧的删起。
func (w *Writer) cleanup() {
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	cutoff := w.now().Add(-w.retention)
	_ = filepath.Walk(w.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
			return nil
		}
		files = append(files, file{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if total > w.maxBytes {
		// 原文占了绝大部分空间，摘要很小却最旧；先删原文，保住摘要（只看摘要也能排查）。
		summary := func(f file) bool { return filepath.Dir(f.path) == w.dir && strings.HasSuffix(f.path, ".jsonl") }
		sort.Slice(files, func(i, j int) bool {
			if si, sj := summary(files[i]), summary(files[j]); si != sj {
				return sj
			}
			return files[i].mod.Before(files[j].mod)
		})
		for _, f := range files {
			if total <= w.maxBytes*9/10 {
				break
			}
			if os.Remove(f.path) == nil {
				total -= f.size
			}
		}
	}
	// 删掉空目录（bodies/日期、incidents/日期/事件）；非空的删不掉，正好。
	var dirs []string
	_ = filepath.Walk(w.dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && path != w.dir {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		if base := filepath.Base(dirs[i]); base != "bodies" && base != "incidents" {
			_ = os.Remove(dirs[i])
		}
	}
}

func writeCapped(path string, data []byte) bool {
	if len(data) > maxBodyBytes {
		data = append(append([]byte(nil), data[:maxBodyBytes]...), []byte("\n...[truncated]\n")...)
	}
	return os.WriteFile(path, data, 0o600) == nil
}

// safeName 把 request id 变成安全的文件名；空的就用时间戳。
func safeName(requestID string, now time.Time) string {
	var b strings.Builder
	for _, r := range requestID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	name := b.String()
	if name == "" || len(name) > 96 {
		name = now.Format("150405.000000000")
	}
	return name
}
