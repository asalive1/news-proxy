package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// US ZIP code -> place data from GeoNames (https://www.geonames.org), licensed CC BY 4.0.
//
//go:embed data/zipcodes.tsv
var zipData []byte

const (
	maxAdhocEntries  = 300
	relayRateLimit   = 60 // requests per client per minute
	maxRelayURLBytes = 2000
	googleNewsHost   = "news.google.com"
)

var (
	zipPlaces     map[string]string
	zipPlacesOnce sync.Once
	zipPattern    = regexp.MustCompile(`^\d{5}(-\d{4})?$`)
	placePattern  = regexp.MustCompile(`^[\p{L}\p{M}0-9 .,'\-]{2,80}$`)
	itemPattern   = regexp.MustCompile(`<item[\s>]`)
)

func zipLookup(zip string) (string, bool) {
	zipPlacesOnce.Do(func() {
		zipPlaces = make(map[string]string, 42000)
		sc := bufio.NewScanner(bytes.NewReader(zipData))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) == 3 {
				zipPlaces[f[0]] = f[1] + ", " + f[2]
			}
		}
	})
	place, ok := zipPlaces[zip]
	return place, ok
}

// resolvePlace turns "48220", "Ferndale, MI" or "Detroit Michigan" into a clean place string.
func resolvePlace(q string) (string, error) {
	q = strings.Join(strings.Fields(q), " ")
	if zipPattern.MatchString(q) {
		place, ok := zipLookup(q[:5])
		if !ok {
			return "", errors.New("that ZIP code was not found")
		}
		return place, nil
	}
	if !placePattern.MatchString(q) {
		return "", errors.New("enter a ZIP code or a city and state, for example Detroit, Michigan")
	}
	return q, nil
}

type adhocEntry struct {
	body        []byte
	contentType string
	fetched     time.Time
}

// adhocCache holds feeds that are not configured sources (local news and relayed user feeds).
type adhocCache struct {
	mu      sync.Mutex
	entries map[string]*adhocEntry
}

func (c *adhocCache) get(key string, ttl time.Duration) *adhocEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil && time.Since(e.fetched) < ttl {
		return e
	}
	return nil
}

// stale returns an expired copy, used when the upstream feed is temporarily down.
func (c *adhocCache) stale(key string) *adhocEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}

func (c *adhocCache) put(key string, e *adhocEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*adhocEntry{}
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxAdhocEntries {
		oldestKey, oldest := "", time.Now()
		for k, v := range c.entries {
			if v.fetched.Before(oldest) {
				oldestKey, oldest = k, v.fetched
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = e
}

// rateLimiter allows a fixed number of calls per client per minute.
type rateLimiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

func (l *rateLimiter) allow(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil || time.Since(l.window) >= time.Minute {
		l.counts, l.window = map[string]int{}, time.Now()
	}
	l.counts[client]++
	return l.counts[client] <= relayRateLimit
}

// fetchAdhoc downloads a feed through the SSRF-safe client without touching the source cache.
func (a *App) fetchAdhoc(target string) (*adhocEntry, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", acceptHeader)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, simplifyFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from the feed", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxFeedBytes {
		return nil, fmt.Errorf("feed is larger than %d MB", maxFeedBytes>>20)
	}
	if !looksLikeFeed(body) {
		return nil, errors.New("the response is not an RSS or Atom feed")
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/xml; charset=utf-8"
	}
	return &adhocEntry{body: body, contentType: ct, fetched: time.Now()}, nil
}

func (a *App) serveAdhoc(w http.ResponseWriter, e *adhocEntry, status string) {
	h := w.Header()
	h.Set("Content-Type", e.contentType)
	h.Set("Cache-Control", "private, max-age=30")
	h.Set("X-Cache-Status", status)
	h.Set("X-Cache-Age", strconv.Itoa(int(time.Since(e.fetched).Seconds())))
	h.Set("Content-Length", strconv.Itoa(len(e.body)))
	_, _ = w.Write(e.body)
}

// handleLocal serves Google News for a place. Only the Google News host is ever contacted.
func (a *App) handleLocal(w http.ResponseWriter, r *http.Request) {
	if handlePublic(w, r) {
		return
	}
	place, err := resolvePlace(r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := "local:" + strings.ToLower(place)
	ttl := a.refreshInterval()
	if e := a.adhoc.get(key, ttl); e != nil {
		a.serveAdhoc(w, e, "HIT")
		return
	}
	lock := a.cache.lock(key)
	lock.Lock()
	defer lock.Unlock()
	if e := a.adhoc.get(key, ttl); e != nil {
		a.serveAdhoc(w, e, "HIT")
		return
	}
	const tail = "?hl=en-US&gl=US&ceid=US:en"
	geo := "https://" + googleNewsHost + "/rss/headlines/section/geo/" + url.PathEscape(place) + tail
	e, err := a.fetchAdhoc(geo)
	if err != nil || len(itemPattern.FindAll(e.body, 3)) < 2 {
		search := "https://" + googleNewsHost + "/rss/search?q=" + url.QueryEscape(place) + "&hl=en-US&gl=US&ceid=US:en"
		if e2, err2 := a.fetchAdhoc(search); err2 == nil {
			e, err = e2, nil
		}
	}
	if err != nil {
		if old := a.adhoc.stale(key); old != nil {
			a.serveAdhoc(w, old, "STALE")
			return
		}
		http.Error(w, "local news unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	a.adhoc.put(key, e)
	a.serveAdhoc(w, e, "MISS")
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// handleRelay fetches a user's own feed on their behalf, so the browser never needs CORS access to it.
// The URL is never logged.
func (a *App) handleRelay(w http.ResponseWriter, r *http.Request) {
	if handlePublic(w, r) {
		return
	}
	if a.getSettings().RelayDisabled {
		http.Error(w, "the feed relay is turned off by the administrator", http.StatusForbidden)
		return
	}
	if !a.limiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxRelayURLBytes || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		http.Error(w, "enter a full http or https feed URL", http.StatusBadRequest)
		return
	}
	if u.User != nil {
		http.Error(w, "URLs that contain a user name or password are not allowed", http.StatusBadRequest)
		return
	}
	key := "relay:" + u.String()
	ttl := a.refreshInterval()
	if e := a.adhoc.get(key, ttl); e != nil {
		a.serveAdhoc(w, e, "HIT")
		return
	}
	e, err := a.fetchAdhoc(u.String())
	if err != nil {
		if old := a.adhoc.stale(key); old != nil {
			a.serveAdhoc(w, old, "STALE")
			return
		}
		http.Error(w, "feed unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	a.adhoc.put(key, e)
	a.serveAdhoc(w, e, "MISS")
}
