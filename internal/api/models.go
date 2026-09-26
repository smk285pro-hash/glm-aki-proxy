package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"glm-aki-proxy/internal/session"
	"glm-aki-proxy/internal/upstream"
)

// ModelInfo describes a model returned by the catalog.
type ModelInfo struct {
	ID           string                 `json:"id"`
	Object       string                 `json:"object"`
	Created      int64                  `json:"created"`
	OwnedBy      string                 `json:"owned_by"`
	Name         string                 `json:"name,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Capabilities map[string]interface{} `json:"capabilities,omitempty"`
}

var (
	modelsCache     []ModelInfo
	modelsCacheTime time.Time
	modelsCacheMu   sync.Mutex
	modelsTTL       = 1 * time.Hour
)

// getFallbackModels returns the standard models list from upstream.KnownModels.
func getFallbackModels() []ModelInfo {
	now := time.Now().Unix()
	out := make([]ModelInfo, 0, len(upstream.KnownModels))
	for _, id := range upstream.KnownModels {
		out = append(out, ModelInfo{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "z.ai",
		})
	}
	return out
}

// FetchModels retrieves live models from Z.AI with 1h in-memory caching.
func (s *Server) FetchModels(ctx context.Context) []ModelInfo {
	modelsCacheMu.Lock()
	if len(modelsCache) > 0 && time.Since(modelsCacheTime) < modelsTTL {
		cached := modelsCache
		modelsCacheMu.Unlock()
		return cached
	}
	modelsCacheMu.Unlock()

	var token string
	if s.sess != nil {
		tok, _, _, _, _ := s.sess.Snapshot()
		token = tok
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, "GET", session.BaseURL+"/api/models", nil)
	if err == nil {
		req.Header.Set("User-Agent", session.ChromeUA)
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := session.SharedClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var parsed struct {
				Data []struct {
					ID          string                 `json:"id"`
					Name        string                 `json:"name"`
					Description string                 `json:"description"`
					Meta        map[string]interface{} `json:"meta"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &parsed) == nil && len(parsed.Data) > 0 {
				now := time.Now().Unix()
				var fresh []ModelInfo
				for _, m := range parsed.Data {
					fresh = append(fresh, ModelInfo{
						ID:           m.ID,
						Object:       "model",
						Created:      now,
						OwnedBy:      "z.ai",
						Name:         m.Name,
						Description:  m.Description,
						Capabilities: m.Meta,
					})
				}
				// Also append Claude alias models for complete client compatibility
				for _, km := range upstream.KnownModels {
					found := false
					for _, f := range fresh {
						if f.ID == km {
							found = true
							break
						}
					}
					if !found {
						fresh = append(fresh, ModelInfo{
							ID:      km,
							Object:  "model",
							Created: now,
							OwnedBy: "z.ai",
						})
					}
				}

				modelsCacheMu.Lock()
				modelsCache = fresh
				modelsCacheTime = time.Now()
				modelsCacheMu.Unlock()
				return fresh
			}
		} else if resp != nil {
			resp.Body.Close()
		}
	}

	log.Printf("[models] live fetch failed, serving fallback catalog")
	fallback := getFallbackModels()
	modelsCacheMu.Lock()
	if len(modelsCache) == 0 {
		modelsCache = fallback
		modelsCacheTime = time.Now()
	}
	modelsCacheMu.Unlock()
	return fallback
}

// ── /v1/models ──

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	models := s.FetchModels(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// ── /admin/models ──

func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	models := s.FetchModels(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":  len(models),
		"models": models,
	})
}
