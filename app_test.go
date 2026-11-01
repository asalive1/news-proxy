package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testFeed = `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item><title>one</title></item></channel></rss>`

func newTestApp(t *testing.T, allowPrivate bool, sources []Source) (*App, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	s := defaultSettings()
	s.AllowPrivateFeeds = allowPrivate
	if err := writeJSONFile(filepath.Join(dir, settingsFileName), s); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(dir, sourcesFileName), sourcesFile{Version: 1, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	return app, app.routes()
}

func do(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFeedCachedAfterFirstFetch(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(testFeed))
	}))
	defer upstream.Close()

	_, h := newTestApp(t, true, []Source{{ID: "demo", Name: "Demo", URL: upstream.URL, Aliases: []string{"legacy"}}})

	first := do(h, "GET", "/feed/demo", "", nil)
	if first.Code != 200 || first.Header().Get("X-Cache-Status") != "MISS" {
		t.Fatalf("first: code=%d status=%q", first.Code, first.Header().Get("X-Cache-Status"))
	}
	second := do(h, "GET", "/feed/demo", "", nil)
	if second.Header().Get("X-Cache-Status") != "HIT" || !strings.Contains(second.Body.String(), "<rss") {
		t.Fatalf("second: status=%q", second.Header().Get("X-Cache-Status"))
	}
	if second.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing CORS header")
	}
	if rec := do(h, "GET", "/legacy", "", nil); rec.Code != 200 {
		t.Fatalf("alias route: %d", rec.Code)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hit %d times, want 1", n)
	}
	if rec := do(h, "GET", "/feed/missing", "", nil); rec.Code != 404 {
		t.Fatalf("unknown source: %d", rec.Code)
	}
}

func TestHealthAndSourcesJSON(t *testing.T) {
	_, h := newTestApp(t, false, []Source{{ID: "a", Name: "A", URL: "https://example.com/rss", Categories: []string{"x"}}})

	rec := do(h, "GET", "/health", "", nil)
	var health map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &health) != nil || health["status"] == nil {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(h, "GET", "/sources.json", "", nil)
	var doc struct {
		Sources []struct {
			ID        string `json:"id"`
			ProxyPath string `json:"proxyPath"`
		} `json:"sources"`
		RefreshIntervalMinutes int `json:"refreshIntervalMinutes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Sources) != 1 || doc.Sources[0].ProxyPath != "/feed/a" || doc.RefreshIntervalMinutes != 10 {
		t.Fatalf("sources.json: %s", rec.Body.String())
	}
}

func TestPrivateFeedsBlockedByDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(testFeed)) }))
	defer upstream.Close()
	_, h := newTestApp(t, false, []Source{{ID: "local", Name: "Local", URL: upstream.URL}})
	if rec := do(h, "GET", "/feed/local", "", nil); rec.Code == 200 {
		t.Fatal("loopback feed should be blocked when private feeds are disallowed")
	}
}

func TestConfigAPIRequiresHeaderAndAddsWithoutRestart(t *testing.T) {
	_, h := newTestApp(t, false, nil)
	body := `{"name":"Example News","url":"https://example.com/rss","categories":["World"," Tech "]}`

	if rec := do(h, "POST", "/config/api/sources", body, map[string]string{"Content-Type": "application/json"}); rec.Code < 400 {
		t.Fatalf("write without CSRF header allowed: %d", rec.Code)
	}
	hdr := map[string]string{"Content-Type": "application/json", "X-Requested-With": "news-proxy"}
	if rec := do(h, "POST", "/config/api/sources", body, hdr); rec.Code >= 300 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/config/api/sources", body, hdr); rec.Code != 409 {
		t.Fatalf("duplicate URL: %d", rec.Code)
	}
	if !strings.Contains(do(h, "GET", "/sources.json", "", nil).Body.String(), "example-news") {
		t.Fatal("new source not discoverable")
	}
	if rec := do(h, "DELETE", "/config/api/sources/example-news", "", hdr); rec.Code >= 300 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if strings.Contains(do(h, "GET", "/sources.json", "", nil).Body.String(), "example-news") {
		t.Fatal("source still listed after delete")
	}
}

func TestAdminPasswordEnforced(t *testing.T) {
	app, h := newTestApp(t, false, nil)
	s := app.getSettings()
	s.AdminPasswordHash = hashPassword("supersecret1")
	if err := app.saveSettings(s); err != nil {
		t.Fatal(err)
	}
	if rec := do(h, "GET", "/config/api/state", "", nil); rec.Code != 401 {
		t.Fatalf("no credentials: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/config/api/state", nil)
	req.SetBasicAuth("admin", "supersecret1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid credentials: %d", rec.Code)
	}
	if rec := do(h, "GET", "/health", "", nil); rec.Code != 200 {
		t.Fatal("/health must stay open")
	}
}

func TestVersionLoaded(t *testing.T) {
	if version == "" || strings.ContainsAny(version, "\r\n ") {
		t.Fatalf("bad version %q", version)
	}
	_, h := newTestApp(t, false, nil)
	if !strings.Contains(do(h, "GET", "/health", "", nil).Body.String(), `"version":"`+version+`"`) {
		t.Fatal("/health missing version")
	}
}

func TestSettingsSanitize(t *testing.T) {
	s := Settings{RefreshIntervalMinutes: 0, Port: 0}
	s.sanitize()
	if s.RefreshIntervalMinutes != defaultRefreshMinutes || s.Port != defaultPort {
		t.Fatalf("got %+v", s)
	}
	_ = os.Getenv
}
