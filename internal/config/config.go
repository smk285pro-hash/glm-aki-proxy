// Package config loads server settings from the environment,
// optionally seeded from a .env file in the working directory.
package config

import (
	"bufio"
	"os"
	"strings"
)

// Config is the full server configuration.
type Config struct {
	Port             string
	Host             string
	AuthToken        string
	ZaiToken         string
	ZaiTokens        []string
	PoolPath         string
	LogLevel         string
	RedisURL         string
	UpstashRestURL   string
	UpstashRestToken string
	RedisKey         string
}

// Load reads .env (if present) then the process environment.
// Explicit environment variables always win over .env values.
func Load() Config {
	applyDotEnv(".env")
	rawTokens := firstOf(os.Getenv("ZAI_TOKENS"), os.Getenv("ZAI_TOKEN_POOL"), os.Getenv("ZAI_TOKEN"))
	var tokens []string
	if rawTokens != "" {
		parts := strings.FieldsFunc(rawTokens, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r' || r == ';'
		})
		for _, p := range parts {
			t := strings.TrimSpace(p)
			if t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	firstTok := ""
	if len(tokens) > 0 {
		firstTok = tokens[0]
	}
	return Config{
		Port:             firstOf(os.Getenv("PORT"), "5084"),
		Host:             firstOf(os.Getenv("HOST"), "127.0.0.1"),
		AuthToken:        firstOf(os.Getenv("PROXY_AUTH_TOKEN"), os.Getenv("AUTH_TOKEN"), os.Getenv("API_KEY"), "aki-local-key"),
		ZaiToken:         firstTok,
		ZaiTokens:        tokens,
		PoolPath:         firstOf(os.Getenv("POOL_PATH"), "tokens.json"),
		LogLevel:         firstOf(os.Getenv("LOG_LEVEL"), "info"),
		RedisURL:         firstOf(os.Getenv("REDIS_URL"), os.Getenv("UPSTASH_REDIS_URL")),
		UpstashRestURL:   strings.TrimSpace(os.Getenv("UPSTASH_REDIS_REST_URL")),
		UpstashRestToken: strings.TrimSpace(os.Getenv("UPSTASH_REDIS_REST_TOKEN")),
		RedisKey:         firstOf(os.Getenv("REDIS_KEY"), "glm_device_tokens"),
	}
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// applyDotEnv parses KEY=VALUE lines and exports the ones that are not
// already set in the environment. No variable expansion, no quotes magic
// beyond stripping one pair of surrounding single/double quotes.
func applyDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

// Addr returns the host:port listen address.
func (c Config) Addr() string { return c.Host + ":" + c.Port }

// Debug reports whether debug logging is on.
func (c Config) Debug() bool { return strings.ToLower(c.LogLevel) == "debug" }
