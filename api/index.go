package handler

import (
	"net/http"
	"sync"

	"glm-aki-proxy/internal/api"
	"glm-aki-proxy/internal/config"
	"glm-aki-proxy/internal/pool"
	"glm-aki-proxy/internal/session"
)

var (
	srv   *api.Server
	srvMu sync.RWMutex
)

func getServer() *api.Server {
	srvMu.RLock()
	s := srv
	srvMu.RUnlock()
	if s != nil {
		return s
	}

	srvMu.Lock()
	defer srvMu.Unlock()
	if srv != nil {
		return srv
	}
	cfg := config.Load()
	tokens, _ := pool.Open(cfg)
	sess := session.NewPool()
	_ = sess.Init(cfg.ZaiTokens)
	srv = api.New(cfg, sess, tokens)
	return srv
}

// Handler is the Vercel Serverless entrypoint.
func Handler(w http.ResponseWriter, r *http.Request) {
	getServer().Handler().ServeHTTP(w, r)
}
