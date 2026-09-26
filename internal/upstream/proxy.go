package upstream

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"glm-aki-proxy/internal/session"
)

// DeadProxyTTL is the isolation duration before a failed proxy is retried.
const DeadProxyTTL = 10 * time.Minute

// ProxyPool manages a rotating pool of HTTP/SOCKS proxies for WAF evasion.
type ProxyPool struct {
	urls     []*url.URL
	idx      atomic.Uint64
	mu       sync.RWMutex
	dead     map[string]time.Time
	useProxy bool
}

// NewProxyPool initializes an empty ProxyPool.
func NewProxyPool() *ProxyPool {
	return &ProxyPool{
		dead: make(map[string]time.Time),
	}
}

// DefaultProxyPool is the singleton proxy pool used across the upstream package.
var DefaultProxyPool = NewProxyPool()

// Load parses proxies from a file path and/or environment string.
// Bare host:port lines default to http://.
func (p *ProxyPool) Load(filePath, envList string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.urls = nil
	var lines []string

	if filePath != "" {
		if data, err := os.ReadFile(filePath); err == nil {
			lines = append(lines, strings.Split(string(data), "\n")...)
		} else if !os.IsNotExist(err) {
			log.Printf("[proxy] warning reading proxy file %s: %v", filePath, err)
		}
	}

	if envList != "" {
		parts := strings.FieldsFunc(envList, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r'
		})
		lines = append(lines, parts...)
	}

	seen := make(map[string]bool)
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !strings.Contains(l, "://") {
			l = "http://" + l
		}
		u, err := url.Parse(l)
		if err != nil || u.Host == "" {
			continue
		}
		key := u.String()
		if !seen[key] {
			seen[key] = true
			p.urls = append(p.urls, u)
		}
	}

	useProxyEnv := strings.ToLower(strings.TrimSpace(os.Getenv("USE_PROXY")))
	if useProxyEnv == "false" || useProxyEnv == "0" {
		p.useProxy = false
	} else if useProxyEnv == "true" || useProxyEnv == "1" || useProxyEnv == "yes" {
		p.useProxy = true
	} else {
		// Default to true if proxies were explicitly provided
		p.useProxy = len(p.urls) > 0
	}

	if len(p.urls) > 0 {
		log.Printf("[proxy] loaded %d proxy routes (enabled=%v)", len(p.urls), p.useProxy)
	}
	return nil
}

// LoadFromEnv loads configuration from USE_PROXY, PROXY_FILE, and PROXY_LIST.
func (p *ProxyPool) LoadFromEnv() error {
	filePath := os.Getenv("PROXY_FILE")
	if filePath == "" {
		filePath = "proxy.txt"
	}
	envList := os.Getenv("PROXY_LIST")
	if envList == "" {
		envList = os.Getenv("PROXIES")
	}
	return p.Load(filePath, envList)
}

// SetEnabled toggles whether proxy rotation is active.
func (p *ProxyPool) SetEnabled(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.useProxy = enabled
}

// Enabled reports whether the pool is configured and active.
func (p *ProxyPool) Enabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.useProxy && len(p.urls) > 0
}

// TotalCount returns the number of proxies loaded.
func (p *ProxyPool) TotalCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.urls)
}

// IsDead reports whether the proxy is currently in the 10-minute dead isolation window.
func (p *ProxyPool) IsDead(u *url.URL) bool {
	if u == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := u.String()
	t, ok := p.dead[key]
	if !ok {
		return false
	}
	if time.Since(t) > DeadProxyTTL {
		delete(p.dead, key)
		return false
	}
	return true
}

// MarkDead isolates the proxy for DeadProxyTTL (10 minutes).
func (p *ProxyPool) MarkDead(u *url.URL) {
	if u == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead[u.String()] = time.Now()
	log.Printf("[proxy] proxy marked dead (10m TTL): %s", u.Redacted())
}

// CurrentURL returns the active proxy, advancing past dead proxies.
func (p *ProxyPool) CurrentURL() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.useProxy || len(p.urls) == 0 {
		return nil
	}

	n := uint64(len(p.urls))
	for i := uint64(0); i < n; i++ {
		u := p.urls[p.idx.Load()%n]
		if !p.isDeadLocked(u) {
			return u
		}
		p.idx.Add(1)
	}
	// If all are marked dead, serve current so pre-check can probe for recovery
	return p.urls[p.idx.Load()%n]
}

func (p *ProxyPool) isDeadLocked(u *url.URL) bool {
	if u == nil {
		return false
	}
	key := u.String()
	t, ok := p.dead[key]
	if !ok {
		return false
	}
	return time.Since(t) <= DeadProxyTTL
}

// Rotate advances to the next proxy in the pool and returns it.
func (p *ProxyPool) Rotate() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.useProxy || len(p.urls) == 0 {
		return nil
	}

	n := uint64(len(p.urls))
	for i := uint64(0); i < n; i++ {
		next := p.idx.Add(1) % n
		u := p.urls[next]
		if !p.isDeadLocked(u) {
			return u
		}
	}
	return p.urls[p.idx.Load()%n]
}

// PreCheck verifies that the proxy route can reach Z.AI without burning a device token.
func (p *ProxyPool) PreCheck(ctx context.Context, u *url.URL, token string) error {
	if u == nil {
		return nil
	}
	client, err := GetBrowserClient(u.String(), 10)
	if err != nil {
		return fmt.Errorf("tls client init error: %w", err)
	}

	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := fhttp.NewRequestWithContext(pctx, "GET", session.BaseURL+"/api/models", nil)
	if err != nil {
		return err
	}
	req.Header = make(fhttp.Header)
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	req.Header.Set("user-agent", ChromeUA)
	req.Header.Set("accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pre-check connection error: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	// Status 2xx, 3xx, or 4xx proves the proxy connected to Z.AI and received an HTTP response
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return nil
	}
	return fmt.Errorf("pre-check status %d", resp.StatusCode)
}

func init() {
	_ = DefaultProxyPool.LoadFromEnv()
}
