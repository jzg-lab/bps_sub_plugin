// Package tools 存取工具调用映射：优先用宿主的 KV（跨副本/重启共享），
// 没有 KV 时退回进程内 LRU（重启丢失，回放走 fallback 重建，功能不塌）。
package tools

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

const (
	// kvNamespace 是本插件在宿主 KV 里的命名空间。
	kvNamespace = "toolcalls"
	// maxValueBytes 是宿主 KV 单值上限，超了就不存（回放走 fallback）。
	maxValueBytes = 256 * 1024
	// memoryCacheSize 是进程内缓存的条数上限。
	memoryCacheSize = 512
	// kvTimeout 是单次 KV 调用的超时。
	kvTimeout = 3 * time.Second
)

// safeKey 限定 KV key 允许的字符（宿主要求 [A-Za-z0-9._-]）。
var safeKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Store 记住原生工具调用，供跨轮回放。并发安全。
type Store struct {
	ttl    time.Duration
	onErr  func()
	mu     sync.Mutex
	cache  map[string]*list.Element
	lru    *list.List
	host   pluginv1.HostServiceClient
	hostMu sync.RWMutex
}

type entry struct {
	key   string
	value map[string]any
}

// New 创建一个存储。onErr 在 KV 出错时调用（用于计数），可为 nil。
func New(ttl time.Duration, onErr func()) *Store {
	if onErr == nil {
		onErr = func() {}
	}
	return &Store{ttl: ttl, onErr: onErr, cache: map[string]*list.Element{}, lru: list.New()}
}

// SetHost 设置宿主 KV 客户端（InitHostServices 成功后调用）；传 nil 表示只用进程内缓存。
func (s *Store) SetHost(host pluginv1.HostServiceClient) {
	s.hostMu.Lock()
	s.host = host
	s.hostMu.Unlock()
}

func (s *Store) hostClient() pluginv1.HostServiceClient {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	return s.host
}

// kvKey 把 call_id 归一成合法 KV key。
func kvKey(callID string) string {
	if len(callID) <= 256 && safeKey.MatchString(callID) {
		return callID
	}
	sum := sha256.Sum256([]byte(callID))
	return "h_" + hex.EncodeToString(sum[:])
}

// RememberNativeCall 记住一个原生调用。先写进程内缓存，再尽力写宿主 KV。
func (s *Store) RememberNativeCall(callID string, native map[string]any) {
	if callID == "" || native == nil {
		return
	}
	s.putMemory(callID, native)

	host := s.hostClient()
	if host == nil {
		return
	}
	value, err := json.Marshal(native)
	if err != nil || len(value) > maxValueBytes {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), kvTimeout)
	defer cancel()
	if _, err := host.KVSet(ctx, &pluginv1.KVSetRequest{
		Namespace:  kvNamespace,
		Key:        kvKey(callID),
		Value:      value,
		TtlSeconds: int64(s.currentTTL() / time.Second),
	}); err != nil {
		s.onErr()
	}
}

func (s *Store) currentTTL() time.Duration {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	return s.ttl
}

// RememberedNativeCall 取回记住的原生调用：先查进程内缓存，未命中再查宿主 KV。
func (s *Store) RememberedNativeCall(callID string) (map[string]any, bool) {
	if callID == "" {
		return nil, false
	}
	if v, ok := s.getMemory(callID); ok {
		return v, true
	}
	host := s.hostClient()
	if host == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), kvTimeout)
	defer cancel()
	resp, err := host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: kvNamespace, Key: kvKey(callID)})
	if err != nil {
		s.onErr()
		return nil, false
	}
	if !resp.GetFound() {
		return nil, false
	}
	var native map[string]any
	if json.Unmarshal(resp.GetValue(), &native) != nil {
		return nil, false
	}
	s.putMemory(callID, native)
	return native, true
}

func (s *Store) putMemory(callID string, native map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.cache[callID]; ok {
		el.Value.(*entry).value = native
		s.lru.MoveToFront(el)
		return
	}
	el := s.lru.PushFront(&entry{key: callID, value: native})
	s.cache[callID] = el
	for s.lru.Len() > memoryCacheSize {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		s.lru.Remove(oldest)
		delete(s.cache, oldest.Value.(*entry).key)
	}
}

func (s *Store) getMemory(callID string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.cache[callID]; ok {
		s.lru.MoveToFront(el)
		return el.Value.(*entry).value, true
	}
	return nil, false
}

// SetTTL 更新写入 KV 时使用的 TTL。
func (s *Store) SetTTL(ttl time.Duration) {
	s.hostMu.Lock()
	s.ttl = ttl
	s.hostMu.Unlock()
}
