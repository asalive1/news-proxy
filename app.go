package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	configPollInterval = 5 * time.Second
	refreshConcurrency = 4
)

type App struct {
	dataDir string
	cache   *Cache
	client  *http.Client
	adhoc   adhocCache
	limiter rateLimiter
	started time.Time
	wake    chan struct{}

	mu             sync.RWMutex
	settings       Settings
	sources        []Source
	settingsMod    time.Time
	sourcesMod     time.Time
	boundAddr      string
	lastRefresh    time.Time
	nextRefresh    time.Time
	envAdminSecret string
}

func NewApp(dataDir string) (*App, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "cache"), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create data directory %s: %w", dataDir, err)
	}
	a := &App{
		dataDir:        dataDir,
		cache:          newCache(filepath.Join(dataDir, "cache")),
		started:        time.Now(),
		wake:           make(chan struct{}, 1),
		envAdminSecret: os.Getenv("ADMIN_PASSWORD"),
	}
	a.client = newFeedClient(func() bool { return a.getSettings().AllowPrivateFeeds })

	settings, existed, err := a.readSettings()
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", settingsFileName, err)
	}
	if !existed {
		if minutes, ok := envInt("REFRESH_INTERVAL_MINUTES"); ok {
			settings.RefreshIntervalMinutes = minutes
		}
		settings.AllowPrivateFeeds = strings.EqualFold(os.Getenv("ALLOW_PRIVATE_FEEDS"), "true")
		settings.sanitize()
		if err := writeJSONFile(a.settingsPath(), settings); err != nil {
			log.Printf("could not write initial settings: %v", err)
		}
	}
	a.settings = settings
	a.boundAddr = listenAddress(settings)

	if err := a.loadSources(); err != nil {
		return nil, err
	}
	for _, src := range a.sources {
		a.cache.loadFromDisk(src.ID)
	}
	a.settingsMod, a.sourcesMod = modTime(a.settingsPath()), modTime(a.sourcesPath())
	return a, nil
}

func (a *App) settingsPath() string { return filepath.Join(a.dataDir, settingsFileName) }
func (a *App) sourcesPath() string  { return filepath.Join(a.dataDir, sourcesFileName) }

func modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func listenAddress(s Settings) string {
	return net.JoinHostPort(s.BindAddress, fmt.Sprint(s.Port))
}

// readSettings loads settings.json and applies the infrastructure environment overrides (PORT, BIND_ADDRESS).
func (a *App) readSettings() (Settings, bool, error) {
	s := defaultSettings()
	existed, err := readJSONFile(a.settingsPath(), &s)
	if err != nil {
		return s, existed, err
	}
	if port, ok := envInt("PORT"); ok {
		s.Port = port
	}
	if bind := os.Getenv("BIND_ADDRESS"); bind != "" {
		s.BindAddress = bind
	}
	s.sanitize()
	return s, existed, nil
}

func (a *App) loadSources() error {
	var file sourcesFile
	existed, err := readJSONFile(a.sourcesPath(), &file)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", sourcesFileName, err)
	}
	if !existed {
		if err := json.Unmarshal(defaultSourcesJSON, &file); err != nil {
			return err
		}
		if err := writeJSONFile(a.sourcesPath(), sourcesFile{Version: 1, Sources: file.Sources}); err != nil {
			log.Printf("could not write initial sources: %v", err)
		}
	}
	a.mu.Lock()
	a.sources = cleanSources(file.Sources)
	a.mu.Unlock()
	return nil
}

// cleanSources drops invalid or duplicate entries from a hand-edited file instead of failing to start.
func cleanSources(in []Source) []Source {
	out := make([]Source, 0, len(in))
	used := map[string]bool{}
	for _, raw := range in {
		src, err := normalizeSource(raw)
		if err != nil {
			log.Printf("ignoring source %q: %v", raw.Name, err)
			continue
		}
		if src.ID == "" {
			src.ID = slugify(src.Name)
		}
		if used[src.ID] || reservedSources[src.ID] {
			log.Printf("ignoring source %q: duplicate or reserved id %q", src.Name, src.ID)
			continue
		}
		used[src.ID] = true
		out = append(out, src)
	}
	return out
}

func (a *App) getSettings() Settings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings
}

func (a *App) getSources() []Source {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Source(nil), a.sources...)
}

func (a *App) findSource(key string) (Source, bool) {
	key = strings.ToLower(key)
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, src := range a.sources {
		if src.ID == key {
			return src, true
		}
	}
	for _, src := range a.sources {
		for _, alias := range src.Aliases {
			if alias == key {
				return src, true
			}
		}
	}
	return Source{}, false
}

func (a *App) refreshInterval() time.Duration {
	return time.Duration(a.getSettings().RefreshIntervalMinutes) * time.Minute
}

func (a *App) wakeLoop() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// saveSources persists the list and makes it live immediately; no restart is involved.
func (a *App) saveSources(sources []Source) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := writeJSONFile(a.sourcesPath(), sourcesFile{Version: 1, Sources: sources}); err != nil {
		return err
	}
	a.sources = sources
	a.sourcesMod = modTime(a.sourcesPath())
	return nil
}

func (a *App) saveSettings(s Settings) error {
	s.sanitize()
	a.mu.Lock()
	defer a.mu.Unlock()
	stored := s
	if err := writeJSONFile(a.settingsPath(), stored); err != nil {
		return err
	}
	a.settings = s
	a.settingsMod = modTime(a.settingsPath())
	return nil
}

// reloadIfChanged picks up hand edits to the JSON files (and installer changes) without a restart.
func (a *App) reloadIfChanged() {
	if mod := modTime(a.sourcesPath()); !mod.IsZero() && !mod.Equal(a.sourcesMod) {
		before := a.getSources()
		if err := a.loadSources(); err != nil {
			log.Printf("sources reload failed: %v", err)
		} else {
			a.sourcesMod = mod
			a.applySourceChanges(before, a.getSources())
		}
	}
	if mod := modTime(a.settingsPath()); !mod.IsZero() && !mod.Equal(a.settingsMod) {
		settings, _, err := a.readSettings()
		if err != nil {
			log.Printf("settings reload failed: %v", err)
			return
		}
		a.mu.Lock()
		a.settingsMod, a.settings = mod, settings
		a.mu.Unlock()
		a.wakeLoop()
		if addr := listenAddress(settings); addr != a.boundAddr {
			log.Printf("listen address changed to %s; restart the service to apply it", addr)
		}
	}
}

func (a *App) applySourceChanges(before, after []Source) {
	keep := map[string]string{}
	for _, src := range after {
		keep[src.ID] = src.URL
	}
	for _, old := range before {
		if url, ok := keep[old.ID]; !ok || url != old.URL {
			a.cache.remove(old.ID)
		}
	}
	for _, src := range after {
		if a.cache.get(src.ID) == nil {
			go func(src Source) { _ = a.fetch(src) }(src)
		}
	}
}

// refreshAll fetches every source (or only stale ones) with limited concurrency.
func (a *App) refreshAll(onlyStale bool) {
	sources := a.getSources()
	interval := a.refreshInterval()
	sem := make(chan struct{}, refreshConcurrency)
	var wg sync.WaitGroup
	for _, src := range sources {
		if onlyStale {
			if e := a.cache.get(src.ID); e != nil && time.Since(e.FetchedAt) < interval {
				continue
			}
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(src Source) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := a.fetch(src); err != nil {
				log.Printf("refresh %s: %v", src.ID, err)
			}
		}(src)
	}
	wg.Wait()
	next := time.Now().Add(interval)
	a.mu.Lock()
	a.lastRefresh = time.Now()
	a.nextRefresh = next
	a.mu.Unlock()
}

func (a *App) refreshLoop(ctx context.Context) {
	a.refreshAll(true)
	poll := time.NewTicker(configPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
			next := time.Now().Add(a.refreshInterval())
			a.mu.Lock()
			a.nextRefresh = next
			a.mu.Unlock()
		case <-poll.C:
			a.reloadIfChanged()
			a.mu.RLock()
			due := !time.Now().Before(a.nextRefresh)
			a.mu.RUnlock()
			if due {
				a.refreshAll(false)
			}
		}
	}
}

func runServer(ctx context.Context, dataDir string) error {
	app, err := NewApp(dataDir)
	if err != nil {
		return err
	}
	settings := app.getSettings()
	listener, err := net.Listen("tcp", app.boundAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s (is the port already in use?): %w", app.boundAddr, err)
	}
	server := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("News-Proxy %s listening on %s (data: %s, refresh every %d min)", version, app.boundAddr, dataDir, settings.RefreshIntervalMinutes)

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	go app.refreshLoop(loopCtx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Printf("News-Proxy stopped")
	return nil
}
