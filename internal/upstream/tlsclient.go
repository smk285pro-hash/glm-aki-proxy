// Package upstream speaks the Z.AI chat protocol: request signing,
// captcha attachment, and the stateful SSE dialect.
package upstream

import (
	"fmt"
	"log"
	"sync"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"glm-aki-proxy/internal/session"
)

// ChromeUA is the desktop browser identity sent on every upstream call.
const ChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// DefaultClientTimeout is the default deadline for direct and auxiliary upstream calls.
const DefaultClientTimeout = 120

// HTTPDoer defines the minimal interface needed to execute fingerprinted HTTP requests.
type HTTPDoer interface {
	Do(req *fhttp.Request) (*fhttp.Response, error)
}

var (
	directClientCache   = map[int]HTTPDoer{}
	directClientCacheMu sync.Mutex
)

// NewBrowserClient builds a fresh Chrome 146 fingerprinted client.
// Proxied clients are instantiated uncached per attempt to prevent socket starvation after rotation.
func NewBrowserClient(proxy string, timeoutSecs int) (HTTPDoer, error) {
	if timeoutSecs <= 0 {
		timeoutSecs = DefaultClientTimeout
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithTimeoutSeconds(timeoutSecs),
	}
	if proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxy))
	}

	client, err := tls_client.NewHttpClient(nil, opts...)
	if err != nil {
		return nil, fmt.Errorf("upstream tls client (proxy=%q): %w", proxy, err)
	}
	return client, nil
}

// GetBrowserClient returns a cached client for direct calls, or a fresh client for proxied calls.
func GetBrowserClient(proxy string, timeoutSecs int) (HTTPDoer, error) {
	if proxy != "" {
		return NewBrowserClient(proxy, timeoutSecs)
	}
	if timeoutSecs <= 0 {
		timeoutSecs = DefaultClientTimeout
	}

	directClientCacheMu.Lock()
	defer directClientCacheMu.Unlock()
	if c, ok := directClientCache[timeoutSecs]; ok {
		return c, nil
	}

	c, err := NewBrowserClient("", timeoutSecs)
	if err != nil {
		return nil, err
	}
	directClientCache[timeoutSecs] = c
	log.Printf("[upstream] direct Chrome 146 TLS client ready (timeout=%ds)", timeoutSecs)
	return c, nil
}

// ResetDirectClient flushes cached clients and rebuilds a fresh Chrome 146 TLS client with clean cookies.
func ResetDirectClient() (HTTPDoer, error) {
	directClientCacheMu.Lock()
	directClientCache = map[int]HTTPDoer{}
	directClientCacheMu.Unlock()

	c, err := GetBrowserClient("", DefaultClientTimeout)
	if err == nil {
		session.RegisterSharedDoer(c)
	}
	return c, err
}

func init() {
	if c, err := GetBrowserClient("", DefaultClientTimeout); err == nil {
		session.RegisterSharedDoer(c)
	}
	session.RegisterClientRefresher(func() {
		_, _ = ResetDirectClient()
	})
}
