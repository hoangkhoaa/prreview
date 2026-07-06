package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"prreview/internal/config"
)

//go:embed ui/index.html
var uiFS embed.FS

// Serve starts the web UI on addr (e.g. "127.0.0.1:8787"). Blocks.
// onShutdown is invoked (in a goroutine, after the response is flushed) when
// the kill switch is pressed; it should terminate the process.
func Serve(addr string, engine *Engine, onShutdown func()) error {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		engine.Shutdown()
		writeJSON(w, map[string]bool{"ok": true})
		if onShutdown != nil {
			go func() {
				time.Sleep(400 * time.Millisecond)
				onShutdown()
			}()
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("per"))
		writeJSON(w, engine.state(q.Get("status"), page, per))
	})

	mux.HandleFunc("POST /api/review", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
			httpErr(w, http.StatusBadRequest, "body must be {\"url\": \"<github PR url>\"}")
			return
		}
		job, err := engine.Submit(strings.TrimSpace(req.URL), "manual")
		if err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, job)
	})

	jobAction := func(fn func(id int64, r *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				httpErr(w, http.StatusBadRequest, "invalid job id")
				return
			}
			if err := fn(id, r); err != nil {
				httpErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
		}
	}

	mux.HandleFunc("POST /api/jobs/{id}/cancel", jobAction(func(id int64, _ *http.Request) error {
		return engine.Cancel(id)
	}))
	mux.HandleFunc("POST /api/jobs/{id}/retry", jobAction(func(id int64, _ *http.Request) error {
		return engine.Retry(id)
	}))
	mux.HandleFunc("DELETE /api/jobs/{id}", jobAction(func(id int64, _ *http.Request) error {
		return engine.Remove(id)
	}))
	mux.HandleFunc("POST /api/jobs/{id}/post", jobAction(func(id int64, r *http.Request) error {
		var req struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return fmt.Errorf("body must be {\"state\": \"APPROVE|COMMENT|REQUEST_CHANGES\"}")
		}
		return engine.Post(id, req.State)
	}))

	mux.HandleFunc("POST /api/purge", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"removed": engine.Purge()})
	})

	mux.HandleFunc("GET /api/repos", func(w http.ResponseWriter, r *http.Request) {
		repos, err := engine.AccessibleRepos()
		if err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"repos": repos})
	})

	mux.HandleFunc("DELETE /api/profiles/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := engine.DeleteProfile(r.PathValue("name")); err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/watch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled     bool `json:"enabled"`
			AutoApprove bool `json:"autoApprove"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		engine.SetWatch(req.Enabled, req.AutoApprove)
		writeJSON(w, engine.Watch())
	})

	mux.HandleFunc("POST /api/watch/filters", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			IncludeTeams bool     `json:"includeTeams"`
			Repos        []string `json:"repos"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := engine.SetWatchFilters(req.IncludeTeams, req.Repos); err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, engine.Watch())
	})

	mux.HandleFunc("POST /api/models", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			BaseURL  string `json:"baseUrl"`
			APIKey   string `json:"apiKey"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		models, err := engine.DiscoverModels(r.Context(), req.Name, req.Provider, req.BaseURL, req.APIKey)
		if err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"models": models})
	})

	mux.HandleFunc("POST /api/test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			BaseURL  string `json:"baseUrl"`
			APIKey   string `json:"apiKey"`
			Model    string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		desc, err := engine.TestConnection(r.Context(), req.Name, req.Provider, req.BaseURL, req.APIKey, req.Model)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": desc})
	})

	mux.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			BaseURL  string `json:"baseUrl"`
			APIKey   string `json:"apiKey"`
			Model    string   `json:"model"`
			Models   []string `json:"models"`
			Depth    string   `json:"depth"`
			Default  bool     `json:"makeDefault"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if len(req.Models) > 0 {
			if err := engine.SetupProfiles(req.Provider, req.BaseURL, req.APIKey,
				req.Depth, req.Models, req.Default); err != nil {
				httpErr(w, http.StatusBadRequest, err.Error())
				return
			}
		} else {
			if err := engine.SetupProfile(req.Name, req.Provider, req.BaseURL,
				req.APIKey, req.Model, req.Depth, req.Default); err != nil {
				httpErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		writeJSON(w, engine.state("", 1, 10))
	})

	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
			Depth string `json:"depth"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if req.Model != "" {
			if err := engine.SetModel(req.Model); err != nil {
				httpErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if req.Depth != "" {
			if err := engine.SetDepth(req.Depth); err != nil {
				httpErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		writeJSON(w, engine.state("", 1, 10))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

type modelView struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"baseUrl"`
	HasKey   bool   `json:"hasKey"`
	Default  bool   `json:"default"`
}

func (e *Engine) state(status string, page, per int) map[string]any {
	base := e.BaseConfig()
	active := e.ActiveConfig()

	var models []modelView
	for name, p := range base.Models {
		models = append(models, modelView{
			Name: name, Provider: p.Provider, Model: p.Model,
			BaseURL: p.BaseURL, HasKey: p.APIKey != "",
			Default: name == base.DefaultModelName,
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })

	if per <= 0 {
		per = 10
	}
	if page < 1 {
		page = 1
	}
	jobs, total, counts := e.JobsPage(status, page, per)
	pages := (total + per - 1) / per
	if pages < 1 {
		pages = 1
	}

	return map[string]any{
		"user":       e.User(),
		"configured": e.Configured(),
		"jobs":       jobs,
		"jobsTotal":  total,
		"jobsCounts": counts,
		"page":       page,
		"pages":      pages,
		"per":        per,
		"watch":      e.Watch(),
		"settings": map[string]any{
			"models":       models,
			"defaultModel": base.DefaultModelName,
			"activeModel":  active.Model,
			"provider":     active.Provider,
			"depth":        active.Depth,
			"keySet":       active.APIKey != "",
			"maskedKey":    config.MaskKey(active.APIKey),
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
