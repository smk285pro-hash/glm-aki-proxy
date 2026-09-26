// Package pool manages single-use captcha device tokens.
// Backends supported:
// 1) Upstash REST API (zero persistent connection overhead, ideal for serverless/Vercel)
// 2) Standard Redis (TCP/TLS via go-redis)
// 3) Local JSON file (default fallback for standalone local usage)
package pool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"glm-aki-proxy/internal/config"
)

// Pool is a token queue backed by Upstash REST, Redis, or disk.
type Pool struct {
	// Disk file backend
	mu     sync.Mutex
	path   string
	tokens []string

	// Upstash REST backend
	restURL   string
	restToken string
	httpCli   *http.Client

	// Standard Redis client backend
	redisCli *redis.Client
	redisKey string
}

type diskFile struct {
	Tokens []string `json:"tokens"`
}

// Open creates a Pool from config. If Upstash REST or Redis is configured,
// it uses the remote store; otherwise it falls back to cfg.PoolPath.
func Open(cfg config.Config) (*Pool, error) {
	key := cfg.RedisKey
	if key == "" {
		key = "glm_device_tokens"
	}

	// 1. Upstash REST (preferred on serverless/Vercel for zero connection pooling overhead)
	if cfg.UpstashRestURL != "" && cfg.UpstashRestToken != "" {
		p := &Pool{
			restURL:   strings.TrimRight(cfg.UpstashRestURL, "/"),
			restToken: cfg.UpstashRestToken,
			redisKey:  key,
			httpCli:   &http.Client{Timeout: 5 * time.Second},
		}
		return p, nil
	}

	// 2. Standard Redis URL (rediss:// or redis://)
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("pool: invalid REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(opt)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Printf("WARN: pool: redis ping failed: %v (will retry on demand)", err)
		}
		return &Pool{
			redisCli: rdb,
			redisKey: key,
		}, nil
	}

	// 3. Fallback to local JSON file
	return OpenFile(cfg.PoolPath)
}

// OpenFile loads tokens from a local JSON file.
func OpenFile(path string) (*Pool, error) {
	if path == "" {
		path = "tokens.json"
	}
	p := &Pool{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	var df diskFile
	if err := json.Unmarshal(raw, &df); err != nil {
		return nil, fmt.Errorf("pool: bad %s: %w", path, err)
	}
	p.tokens = append([]string(nil), df.Tokens...)
	return p, nil
}

// Backend reports the active storage backend.
func (p *Pool) Backend() string {
	if p.restURL != "" && p.restToken != "" {
		return "upstash-rest"
	}
	if p.redisCli != nil {
		return "redis"
	}
	return "file"
}

// MaxTokenAge defines how long a device token is considered valid before being auto-discarded.
const MaxTokenAge = 3 * time.Hour

func tokenTimestamp(deviceToken string) (time.Time, bool) {
	raw, err := base64.StdEncoding.DecodeString(deviceToken)
	if err != nil {
		return time.Time{}, false
	}
	parts := strings.SplitN(string(raw), "#", 3)
	if len(parts) < 2 {
		return time.Time{}, false
	}
	sub := strings.SplitN(parts[1], "-h-", 2)
	if len(sub) < 2 {
		return time.Time{}, false
	}
	tsStr := strings.SplitN(sub[1], "-", 2)[0]
	ms, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// Take removes and returns the oldest valid token. ok=false when empty.
// Stale tokens older than MaxTokenAge are automatically purged.
func (p *Pool) Take() (tok string, ok bool) {
	for attempt := 0; attempt < 50; attempt++ {
		var candidate string
		var found bool
		if p.restURL != "" && p.restToken != "" {
			candidate, found = p.takeRest()
		} else if p.redisCli != nil {
			candidate, found = p.takeRedis()
		} else {
			candidate, found = p.takeFile()
		}
		if !found || candidate == "" {
			return "", false
		}
		if ts, ok := tokenTimestamp(candidate); ok {
			age := time.Since(ts)
			if age > MaxTokenAge {
				log.Printf("[pool] auto-discarded expired token (age: %s)", age.Round(time.Minute))
				continue
			}
		}
		return candidate, true
	}
	return "", false
}

// Count reports remaining tokens.
func (p *Pool) Count() int {
	if p.restURL != "" && p.restToken != "" {
		return p.countRest()
	}
	if p.redisCli != nil {
		return p.countRedis()
	}
	return p.countFile()
}

// Push adds tokens to the pool and returns the new total count.
func (p *Pool) Push(tokens ...string) (int, error) {
	if p.restURL != "" && p.restToken != "" {
		return p.pushRest(tokens)
	}
	if p.redisCli != nil {
		return p.pushRedis(tokens)
	}
	return p.pushFile(tokens)
}

// ── Upstash REST implementation ──

func (p *Pool) takeRest() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _ := json.Marshal([]string{"LPOP", p.redisKey})
	req, err := http.NewRequestWithContext(ctx, "POST", p.restURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("pool upstash-rest: request build failed: %v", err)
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+p.restToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpCli.Do(req)
	if err != nil {
		log.Printf("pool upstash-rest: request failed: %v", err)
		return "", false
	}
	defer resp.Body.Close()
	var res struct {
		Result *string `json:"result"`
		Error  *string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		log.Printf("pool upstash-rest: decode failed: %v", err)
		return "", false
	}
	if res.Error != nil && *res.Error != "" {
		log.Printf("pool upstash-rest: error: %s", *res.Error)
		return "", false
	}
	if res.Result == nil || *res.Result == "" {
		return "", false
	}
	return *res.Result, true
}

func (p *Pool) countRest() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _ := json.Marshal([]string{"LLEN", p.redisKey})
	req, err := http.NewRequestWithContext(ctx, "POST", p.restURL, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+p.restToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpCli.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var res struct {
		Result int `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res.Result
}

func (p *Pool) pushRest(tokens []string) (int, error) {
	if len(tokens) == 0 {
		return p.countRest(), nil
	}
	const chunkSize = 200
	total := 0
	for i := 0; i < len(tokens); i += chunkSize {
		end := i + chunkSize
		if end > len(tokens) {
			end = len(tokens)
		}
		chunk := tokens[i:end]
		cmd := make([]string, 0, len(chunk)+2)
		cmd = append(cmd, "RPUSH", p.redisKey)
		cmd = append(cmd, chunk...)
		body, _ := json.Marshal(cmd)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", p.restURL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+p.restToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.httpCli.Do(req)
		if err != nil {
			cancel()
			return 0, err
		}
		var res struct {
			Result int     `json:"result"`
			Error  *string `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		cancel()
		if err != nil {
			return 0, err
		}
		if res.Error != nil && *res.Error != "" {
			return 0, fmt.Errorf("upstash error: %s", *res.Error)
		}
		total = res.Result
	}
	return total, nil
}

// ── Standard Redis implementation ──

func (p *Pool) takeRedis() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, err := p.redisCli.LPop(ctx, p.redisKey).Result()
	if err == redis.Nil {
		return "", false
	}
	if err != nil {
		log.Printf("pool redis lpop: %v", err)
		return "", false
	}
	return val, true
}

func (p *Pool) countRedis() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := p.redisCli.LLen(ctx, p.redisKey).Result()
	if err != nil {
		return 0
	}
	return int(n)
}

func (p *Pool) pushRedis(tokens []string) (int, error) {
	if len(tokens) == 0 {
		return p.countRedis(), nil
	}
	const chunkSize = 200
	var total int64
	for i := 0; i < len(tokens); i += chunkSize {
		end := i + chunkSize
		if end > len(tokens) {
			end = len(tokens)
		}
		chunk := tokens[i:end]
		vals := make([]interface{}, len(chunk))
		for idx, v := range chunk {
			vals[idx] = v
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		n, err := p.redisCli.RPush(ctx, p.redisKey, vals...).Result()
		cancel()
		if err != nil {
			return 0, err
		}
		total = n
	}
	return int(total), nil
}

// ── Local disk file implementation ──

func (p *Pool) takeFile() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tokens) == 0 {
		return "", false
	}
	tok := p.tokens[0]
	p.tokens = append([]string(nil), p.tokens[1:]...)
	if err := p.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "pool: persist failed: %v\n", err)
	}
	return tok, true
}

func (p *Pool) countFile() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokens)
}

func (p *Pool) pushFile(tokens []string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = append(p.tokens, tokens...)
	if err := p.persist(); err != nil {
		return 0, err
	}
	return len(p.tokens), nil
}

func (p *Pool) persist() error {
	raw, err := json.MarshalIndent(diskFile{Tokens: p.tokens}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, append(raw, '\n'), 0600)
}
