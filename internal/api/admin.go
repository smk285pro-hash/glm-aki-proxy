package api

import (
	"net/http"
	"time"

	"glm-aki-proxy/internal/upstream"
)

var serverStartTime = time.Now()

// ── /admin/health ──

func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	total := 0
	healthy := 0
	if s.sess != nil {
		total = s.sess.TotalCount()
		healthy = s.sess.HealthyCount()
	}
	status := "healthy"
	if healthy == 0 && total > 0 {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":           status,
		"uptime_seconds":   int64(time.Since(serverStartTime).Seconds()),
		"total_sessions":   total,
		"healthy_sessions": healthy,
		"timestamp":        time.Now().Unix(),
	})
}

// ── /admin/stats ──

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	total := 0
	healthy := 0
	if s.sess != nil {
		total = s.sess.TotalCount()
		healthy = s.sess.HealthyCount()
	}
	s.mu.Lock()
	activeConversations := len(s.chats)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_sessions":       total,
		"healthy_sessions":     healthy,
		"active_conversations": activeConversations,
		"pacing_min_ms":        upstream.GetMinGap().Milliseconds(),
		"timestamp":            time.Now().Unix(),
	})
}

// ── /admin/session/clear ──

func (s *Server) handleSessionClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	s.mu.Lock()
	count := len(s.chats)
	s.chats = make(map[string]*conversation)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "cleared",
		"cleared": count,
	})
}
