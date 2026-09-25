// Package transport 负责把宿主交来的出站请求真实发往上游，并管理连接池。
package transport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
)

// maxPooledProxies 限制按代理区分的连接池数量，避免账号代理很多时无限增长。
const maxPooledProxies = 256

// Pool 按代理地址复用 http.Transport。配置切换时整体换新并关闭旧的空闲连接。
type Pool struct {
	mu      sync.Mutex
	cfg     config.Config
	entries map[string]*poolEntry
}

type poolEntry struct {
	transport *http.Transport
	lastUsed  time.Time
}

// NewPool 用给定配置创建连接池。
func NewPool(cfg config.Config) *Pool {
	return &Pool{cfg: cfg, entries: make(map[string]*poolEntry)}
}

// Config 返回当前生效配置的副本。
func (p *Pool) Config() config.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Clone()
}

// Apply 切换配置。已在进行中的请求继续使用旧 Transport 直到完成。
func (p *Pool) Apply(cfg config.Config) {
	p.mu.Lock()
	old := p.entries
	p.cfg = cfg.Clone()
	p.entries = make(map[string]*poolEntry)
	p.mu.Unlock()
	for _, entry := range old {
		entry.transport.CloseIdleConnections()
	}
}

// Close 关闭所有空闲连接。
func (p *Pool) Close() {
	p.Apply(p.Config())
}

// Get 返回指定代理对应的 Transport。proxyURL 为空或配置禁用账号代理时直连。
func (p *Pool) Get(proxyURL string) (*http.Transport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cfg.UseAccountProxy {
		proxyURL = ""
	}
	if entry, ok := p.entries[proxyURL]; ok {
		entry.lastUsed = time.Now()
		return entry.transport, nil
	}
	var proxy *url.URL
	if proxyURL != "" {
		parsed, err := parseProxyURL(proxyURL)
		if err != nil {
			return nil, err
		}
		proxy = parsed
	}
	if len(p.entries) >= maxPooledProxies {
		p.evictOldestLocked()
	}
	entry := &poolEntry{transport: newTransport(p.cfg, proxy), lastUsed: time.Now()}
	p.entries[proxyURL] = entry
	return entry.transport, nil
}

func (p *Pool) evictOldestLocked() {
	var oldestKey string
	var oldest *poolEntry
	for key, entry := range p.entries {
		if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
			oldestKey, oldest = key, entry
		}
	}
	if oldest != nil {
		delete(p.entries, oldestKey)
		oldest.transport.CloseIdleConnections()
	}
}

func parseProxyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("账号代理地址无效")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return parsed, nil
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", parsed.Scheme)
	}
}

func newTransport(cfg config.Config, proxy *url.URL) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     cfg.EnableHTTP2,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeoutSeconds) * time.Second,
		IdleConnTimeout:       time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxIdleConns:          cfg.MaxIdleConnsPerHost * 4,
		// 原样透传：不自动加 Accept-Encoding，也不自动解压，由宿主决定。
		DisableCompression: true,
	}
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	if !cfg.EnableHTTP2 {
		// 非 nil 的空映射会关闭 HTTP/2 自动协商。
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return transport
}
