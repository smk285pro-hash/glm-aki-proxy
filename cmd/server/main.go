// Command server runs the glm-aki-proxy HTTP bridge.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"glm-aki-proxy/internal/api"
	"glm-aki-proxy/internal/config"
	"glm-aki-proxy/internal/pool"
	"glm-aki-proxy/internal/session"
)

func main() {
	cfg := config.Load()

	tokens, err := pool.Open(cfg)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	if tokens.Count() == 0 {
		log.Printf("WARN: pool [%s] is empty — collect tokens first (see README)", tokens.Backend())
	} else {
		log.Printf("pool backend: %s (remaining: %d tokens)", tokens.Backend(), tokens.Count())
	}

	sess := session.NewPool()
	if err := sess.Init(cfg.ZaiTokens); err != nil {
		log.Fatalf("session: %v", err)
	}

	srv := api.New(cfg, sess, tokens)
	addr := cfg.Addr()
	log.Printf("glm-aki-proxy listening on %s (pool: %d tokens)", addr, tokens.Count())

	// No WriteTimeout: it would kill long-lived SSE streams. The
	// upstream watchdog bounds stalls instead. ReadHeaderTimeout alone
	// guards slow-loris style header drips.
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()
	<-done
	log.Println("shutting down (draining up to 10s)")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
