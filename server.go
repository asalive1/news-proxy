package main

import (
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/config.html
var configPage []byte

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/sources.json", a.handleSources)
	mux.HandleFunc("/feed/{id}", a.handleFeed)

	mux.HandleFunc("/config", a.admin(a.handleConfigPage))
	mux.HandleFunc("/config/{$}", a.admin(a.handleConfigPage))
	mux.HandleFunc("GET /config/api/state", a.admin(a.handleState))
	mux.HandleFunc("POST /config/api/sources", a.admin(a.handleAddSource))
	mux.HandleFunc("PUT /config/api/sources/{id}", a.admin(a.handleUpdateSource))
	mux.HandleFunc("DELETE /config/api/sources/{id}", a.admin(a.handleDeleteSource))
	mux.HandleFunc("POST /config/api/sources/{id}/refresh", a.admin(a.handleRefreshSource))
	mux.HandleFunc("POST /config/api/refresh", a.admin(a.handleRefreshAll))
	mux.HandleFunc("PUT /config/api/settings", a.admin(a.handleSettings))

	// Anything else may be a legacy path such as /bbc (the old nginx layout) or a 404.
	mux.HandleFunc("/", a.handleRoot)
	return mux
}

func setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "*")
	h.Set("Access-Control-Expose-Headers", "X-Cache-Status, X-Cache-Age")
}

// handlePublic answers CORS preflights and rejects non-read methods. Returns true if the request is finished.
func handlePublic(w http.ResponseWriter, r *http.Request) bool {
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if handlePublic(w, r) {
		return
	}
	sources := a.getSources()
	cached := 0
	for _, src := range sources {
		if a.cache.get(src.ID) != nil {
			cached++
		}
	}
	a.mu.RLock()
	last, next := a.lastRefresh, a.nextRefresh
	a.mu.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "ok",
		"version":                version,
		"uptimeSeconds":          int64(time.Since(a.started).Seconds()),
		"sources":                len(sources),
		"cachedSources":          cached,
		"refreshIntervalMinutes": a.getSettings().RefreshIntervalMinutes,
		"lastRefresh":            nullableTime(last),
		"nextRefresh":            nullableTime(next),
	})
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

type catalogSource struct {
	Source
	ProxyPath string `json:"proxyPath"`
}

// handleSources is the discovery document the widget downloads to learn which feeds exist.
func (a *App) handleSources(w http.ResponseWriter, r *http.Request) {
	if handlePublic(w, r) {
		return
	}
	sources := a.getSources()
	catalog := make([]catalogSource, 0, len(sources))
	for _, src := range sources {
		src.Aliases = nil
		catalog = append(catalog, catalogSource{Source: src, ProxyPath: feedPath(src.ID)})
	}
	body, _ := json.Marshal(map[string]any{
		"version":                1,
		"refreshIntervalMinutes": a.getSettings().RefreshIntervalMinutes,
		"sources":                catalog,
	})
	etag := hashBody(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	if handlePublic(w, r) {
		return
	}
	src, ok := a.findSource(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown feed", http.StatusNotFound)
		return
	}
	a.serveFeed(w, r, src)
}

func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if handlePublic(w, r) {
		return
	}
	key := strings.Trim(r.URL.Path, "/")
	if key == "" {
		http.Redirect(w, r, "/config", http.StatusFound)
		return
	}
	if src, ok := a.findSource(key); ok && !strings.Contains(key, "/") {
		a.serveFeed(w, r, src)
		return
	}
	http.NotFound(w, r)
}

func (a *App) serveFeed(w http.ResponseWriter, r *http.Request, src Source) {
	cacheStatus := "HIT"
	e := a.cache.get(src.ID)
	if e == nil {
		cacheStatus = "MISS"
		if err := a.fetch(src); err != nil {
			http.Error(w, "feed unavailable: "+err.Error(), http.StatusBadGateway)
			return
		}
		if e = a.cache.get(src.ID); e == nil {
			http.Error(w, "feed unavailable", http.StatusBadGateway)
			return
		}
	} else if time.Since(e.FetchedAt) > 3*a.refreshInterval() {
		cacheStatus = "STALE"
	}
	a.cache.stat(src.ID).hits.Add(1)

	h := w.Header()
	h.Set("Content-Type", e.ContentType)
	h.Set("ETag", e.Hash)
	h.Set("Cache-Control", "public, max-age=30")
	h.Set("X-Cache-Status", cacheStatus)
	h.Set("X-Cache-Age", strconv.Itoa(int(time.Since(e.FetchedAt).Seconds())))
	h.Add("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == e.Hash {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write(e.Body)
		_ = zw.Close()
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(e.Body)))
	_, _ = w.Write(e.Body)
}

func (a *App) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
	_, _ = w.Write(configPage)
}

// admin protects /config: optional HTTP basic auth, plus a custom header on writes so another website
// cannot make a browser change settings on the user's behalf (a cross-site request cannot set it).
func (a *App) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="News-Proxy configuration"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "news-proxy" {
			http.Error(w, "missing X-Requested-With header", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (a *App) authorized(r *http.Request) bool {
	hash := a.getSettings().AdminPasswordHash
	if hash == "" && a.envAdminSecret == "" {
		return true
	}
	user, password, ok := r.BasicAuth()
	if !ok || user != "admin" {
		return false
	}
	if a.envAdminSecret != "" && password == a.envAdminSecret {
		return true
	}
	return hash != "" && checkPassword(hash, password)
}

func errorJSON(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		errorJSON(w, http.StatusBadRequest, "Invalid request body.")
		return false
	}
	return true
}

type sourceView struct {
	Source
	ProxyPath   string     `json:"proxyPath"`
	Cached      bool       `json:"cached"`
	Bytes       int        `json:"bytes"`
	AgeSeconds  *int64     `json:"ageSeconds"`
	LastAttempt *time.Time `json:"lastAttempt"`
	LastError   string     `json:"lastError"`
	Hits        int64      `json:"hits"`
}

func (a *App) viewOf(src Source) sourceView {
	view := sourceView{Source: src, ProxyPath: feedPath(src.ID)}
	if e := a.cache.get(src.ID); e != nil {
		age := int64(time.Since(e.FetchedAt).Seconds())
		view.Cached, view.Bytes, view.AgeSeconds = true, len(e.Body), &age
	}
	snap := a.cache.snapshot(src.ID)
	view.LastAttempt, view.LastError, view.Hits = nullableTime(snap.LastAttempt), snap.LastError, snap.Hits
	return view
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	sources := a.getSources()
	views := make([]sourceView, 0, len(sources))
	for _, src := range sources {
		views = append(views, a.viewOf(src))
	}
	s := a.getSettings()
	a.mu.RLock()
	last, next := a.lastRefresh, a.nextRefresh
	a.mu.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version,
		"settings": map[string]any{
			"port":                   s.Port,
			"bindAddress":            s.BindAddress,
			"refreshIntervalMinutes": s.RefreshIntervalMinutes,
			"allowPrivateFeeds":      s.AllowPrivateFeeds,
			"passwordSet":            s.AdminPasswordHash != "" || a.envAdminSecret != "",
			"passwordFromEnv":        a.envAdminSecret != "",
		},
		"sources":     views,
		"lastRefresh": nullableTime(last),
		"nextRefresh": nullableTime(next),
	})
}

func (a *App) uniqueID(base string, sources []Source, skip string) string {
	taken := func(id string) bool {
		if reservedSources[id] {
			return true
		}
		for _, src := range sources {
			if src.ID == id && src.ID != skip {
				return true
			}
			for _, alias := range src.Aliases {
				if alias == id {
					return true
				}
			}
		}
		return false
	}
	id := base
	for n := 2; taken(id); n++ {
		suffix := fmt.Sprintf("-%d", n)
		id = strings.TrimRight(base[:min(len(base), 64-len(suffix))], "-") + suffix
	}
	return id
}

func (a *App) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var in Source
	if !decodeBody(w, r, &in) {
		return
	}
	src, err := normalizeSource(in)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	sources := a.getSources()
	for _, existing := range sources {
		if strings.EqualFold(existing.URL, src.URL) {
			errorJSON(w, http.StatusConflict, "This feed URL is already in the list as \""+existing.Name+"\".")
			return
		}
	}
	if src.ID == "" {
		src.ID = slugify(src.Name)
	}
	if reservedSources[src.ID] || sourceIDTaken(sources, src.ID) {
		if in.ID != "" {
			errorJSON(w, http.StatusConflict, "That id is already in use.")
			return
		}
		src.ID = a.uniqueID(src.ID, sources, "")
	}
	if err := a.saveSources(append(sources, src)); err != nil {
		errorJSON(w, http.StatusInternalServerError, "Could not save: "+err.Error())
		return
	}
	go func() { _ = a.fetch(src) }()
	writeJSON(w, http.StatusCreated, a.viewOf(src))
}

func sourceIDTaken(sources []Source, id string) bool {
	for _, src := range sources {
		if src.ID == id {
			return true
		}
		for _, alias := range src.Aliases {
			if alias == id {
				return true
			}
		}
	}
	return false
}

func (a *App) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	var in Source
	if !decodeBody(w, r, &in) {
		return
	}
	updated, err := normalizeSource(in)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	sources := a.getSources()
	index := -1
	for i, src := range sources {
		if src.ID == id {
			index = i
		} else if strings.EqualFold(src.URL, updated.URL) {
			errorJSON(w, http.StatusConflict, "This feed URL is already in the list as \""+src.Name+"\".")
			return
		}
	}
	if index < 0 {
		errorJSON(w, http.StatusNotFound, "Source not found.")
		return
	}
	previous := sources[index]
	updated.ID, updated.Aliases = previous.ID, previous.Aliases // the id is the public path and must stay stable
	sources[index] = updated
	if err := a.saveSources(sources); err != nil {
		errorJSON(w, http.StatusInternalServerError, "Could not save: "+err.Error())
		return
	}
	if previous.URL != updated.URL {
		a.cache.remove(updated.ID)
		go func() { _ = a.fetch(updated) }()
	}
	writeJSON(w, http.StatusOK, a.viewOf(updated))
}

func (a *App) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	sources := a.getSources()
	kept := make([]Source, 0, len(sources))
	for _, src := range sources {
		if src.ID != id {
			kept = append(kept, src)
		}
	}
	if len(kept) == len(sources) {
		errorJSON(w, http.StatusNotFound, "Source not found.")
		return
	}
	if err := a.saveSources(kept); err != nil {
		errorJSON(w, http.StatusInternalServerError, "Could not save: "+err.Error())
		return
	}
	a.cache.remove(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleRefreshSource(w http.ResponseWriter, r *http.Request) {
	src, ok := a.findSource(r.PathValue("id"))
	if !ok {
		errorJSON(w, http.StatusNotFound, "Source not found.")
		return
	}
	if err := a.fetch(src); err != nil {
		errorJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewOf(src))
}

func (a *App) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	go a.refreshAll(false)
	w.WriteHeader(http.StatusAccepted)
}

type settingsUpdate struct {
	RefreshIntervalMinutes int    `json:"refreshIntervalMinutes"`
	AllowPrivateFeeds      bool   `json:"allowPrivateFeeds"`
	NewPassword            string `json:"newPassword"`
	ClearPassword          bool   `json:"clearPassword"`
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var in settingsUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	if in.RefreshIntervalMinutes < 1 || in.RefreshIntervalMinutes > maxRefreshMinutes {
		errorJSON(w, http.StatusBadRequest, fmt.Sprintf("The refresh interval must be between 1 and %d minutes.", maxRefreshMinutes))
		return
	}
	s := a.getSettings()
	s.RefreshIntervalMinutes, s.AllowPrivateFeeds = in.RefreshIntervalMinutes, in.AllowPrivateFeeds
	switch {
	case in.NewPassword != "":
		if len(in.NewPassword) < 8 {
			errorJSON(w, http.StatusBadRequest, "The admin password must be at least 8 characters.")
			return
		}
		s.AdminPasswordHash = hashPassword(in.NewPassword)
	case in.ClearPassword:
		s.AdminPasswordHash = ""
	}
	if err := a.saveSettings(s); err != nil {
		errorJSON(w, http.StatusInternalServerError, "Could not save: "+err.Error())
		return
	}
	a.wakeLoop()
	a.handleState(w, r)
}
