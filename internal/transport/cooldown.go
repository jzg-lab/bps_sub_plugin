package transport

import (
	"sync"
	"time"
)

// policyCooldown 是被 basispoints 以 usage policy 拒绝的 ChatGPT 账号在冷却期内直接走 codex 的时长。
// 这种封禁是账号级的（同一账号发任何请求都 403），继续发只会被宿主累计 403 停号。
const policyCooldown = 24 * time.Hour

// Cooldown 记录暂时不走 basispoints 的 ChatGPT 账号（键为 chatgpt-account-id），只存在内存里，
// 插件重启即清空。零值可用。
type Cooldown struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

func (c *Cooldown) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Add 让账号从现在起冷却 d。
func (c *Cooldown) Add(account string, d time.Duration) {
	if account == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = map[string]time.Time{}
	}
	c.until[account] = c.clock().Add(d)
}

// Active 判断账号是否在冷却中，顺带清掉已过期的记录。
func (c *Cooldown) Active(account string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[account]
	if !ok {
		return false
	}
	if c.clock().Before(until) {
		return true
	}
	delete(c.until, account)
	return false
}

// Len 返回冷却中的账号数（顺带清理过期记录）。
func (c *Cooldown) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for account, until := range c.until {
		if !now.Before(until) {
			delete(c.until, account)
		}
	}
	return len(c.until)
}
