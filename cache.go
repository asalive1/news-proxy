package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxFeedBytes   = 10 << 20
	fetchTimeout   = 25 * time.Second
	acceptHeader   = "application/rss+xml, application/atom+xml, application/xml, text/xml;q=0.9, */*;q=0.8"
	maxRedirects   = 5
	feedProbeBytes = 64 << 10
)

var userAgent = "Mozilla/5.0 (compatible; AuroraNewsProxy/" + version + ")"

type entry struct {
	Body         []byte
	ContentType  string
	ETag         string // upstream validator, used for conditional requests
	LastModified string
	Hash         string // our own ETag for clients
	FetchedAt    time.Time
}

type status struct {
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
	hits        atomic.Int64
}

type diskMeta struct {
	ContentType  string    `json:"contentType"`
	ETag         string    `json:"etag"`
	LastModified string    `json:"lastModified"`
	FetchedAt    time.Time `json:"fetchedAt"`
}

// Cache keeps the latest good copy of every feed in memory and on disk (so a restart serves instantly).
type Cache struct {
	dir     string
	mu      sync.RWMutex
	entries map[string]*entry
	stats   map[string]*status
	locks   sync.Map // id -> *sync.Mutex, serialises fetches for one source
}

func newCache(dir string) *Cache {
	_ = os.MkdirAll(dir, 0o755)
	return &Cache{dir: dir, entries: map[string]*entry{}, stats: map[string]*status{}}
}

func (c *Cache) lock(id string) *sync.Mutex {
	m, _ := c.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (c *Cache) get(id string) *entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[id]
}

func (c *Cache) stat(id string) *status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats[id]
	if s == nil {
		s = &status{}
		c.stats[id] = s
	}
	return s
}

type statusSnapshot struct {
	LastAttempt, LastSuccess time.Time
	LastError                string
	Hits                     int64
}

func (c *Cache) snapshot(id string) statusSnapshot {
	s := c.stat(id)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return statusSnapshot{s.LastAttempt, s.LastSuccess, s.LastError, s.hits.Load()}
}

func (c *Cache) put(id string, e *entry) {
	c.mu.Lock()
	c.entries[id] = e
	s := c.stats[id]
	if s == nil {
		s = &status{}
		c.stats[id] = s
	}
	s.LastAttempt, s.LastSuccess, s.LastError = e.FetchedAt, e.FetchedAt, ""
	c.mu.Unlock()
	c.persist(id, e)
}

func (c *Cache) recordError(id string, err error) {
	s := c.stat(id)
	c.mu.Lock()
	s.LastAttempt, s.LastError = time.Now(), err.Error()
	c.mu.Unlock()
}

func (c *Cache) remove(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	delete(c.stats, id)
	c.mu.Unlock()
	_ = os.Remove(c.bodyPath(id))
	_ = os.Remove(c.metaPath(id))
}

func (c *Cache) bodyPath(id string) string { return filepath.Join(c.dir, id+".xml") }
func (c *Cache) metaPath(id string) string { return filepath.Join(c.dir, id+".json") }

func (c *Cache) persist(id string, e *entry) {
	if err := os.WriteFile(c.bodyPath(id), e.Body, 0o644); err != nil {
		return
	}
	_ = writeJSONFile(c.metaPath(id), diskMeta{e.ContentType, e.ETag, e.LastModified, e.FetchedAt})
}

func (c *Cache) loadFromDisk(id string) {
	body, err := os.ReadFile(c.bodyPath(id))
	if err != nil {
		return
	}
	var meta diskMeta
	if _, err := readJSONFile(c.metaPath(id), &meta); err != nil {
		return
	}
	e := &entry{Body: body, ContentType: meta.ContentType, ETag: meta.ETag, LastModified: meta.LastModified, FetchedAt: meta.FetchedAt, Hash: hashBody(body)}
	c.mu.Lock()
	c.entries[id] = e
	c.mu.Unlock()
	s := c.stat(id)
	c.mu.Lock()
	s.LastSuccess = meta.FetchedAt
	c.mu.Unlock()
}

func hashBody(body []byte) string {
	sum := sha1.Sum(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func looksLikeFeed(body []byte) bool {
	probe := body
	if len(probe) > feedProbeBytes {
		probe = probe[:feedProbeBytes]
	}
	text := strings.ToLower(string(probe))
	return strings.Contains(text, "<rss") || strings.Contains(text, "<feed") || strings.Contains(text, "<rdf:rdf")
}

// blockedIP implements the SSRF policy: metadata/link-local, unspecified and multicast are never allowed;
// loopback and private ranges only when the admin opts in (internal feeds).
func blockedIP(ip net.IP, allowPrivate bool) bool {
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return !allowPrivate
	}
	return false
}

func usesEnvProxy() bool {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func newFeedClient(allowPrivate func() bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !usesEnvProxy() {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && blockedIP(ip, allowPrivate()) {
				return fmt.Errorf("address %s is not allowed (enable \"Allow private/internal feeds\" to permit it)", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          20,
			IdleConnTimeout:       60 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to unsupported scheme")
			}
			return nil
		},
	}
}

// fetch downloads one source using conditional requests so unchanged feeds cost almost no bandwidth.
func (a *App) fetch(src Source) error {
	c := a.cache
	lock := c.lock(src.ID)
	lock.Lock()
	defer lock.Unlock()
	err := a.fetchLocked(src)
	if err != nil {
		c.recordError(src.ID, err)
	}
	return err
}

func (a *App) fetchLocked(src Source) error {
	req, err := http.NewRequest(http.MethodGet, src.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", acceptHeader)
	old := a.cache.get(src.ID)
	if old != nil {
		if old.ETag != "" {
			req.Header.Set("If-None-Match", old.ETag)
		}
		if old.LastModified != "" {
			req.Header.Set("If-Modified-Since", old.LastModified)
		}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return simplifyFetchError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && old != nil {
		refreshed := *old
		refreshed.FetchedAt = time.Now()
		a.cache.put(src.ID, &refreshed)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from the feed", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxFeedBytes {
		return fmt.Errorf("feed is larger than %d MB", maxFeedBytes>>20)
	}
	if !looksLikeFeed(body) {
		return errors.New("the response is not an RSS or Atom feed")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/xml; charset=utf-8"
	}
	a.cache.put(src.ID, &entry{
		Body: body, ContentType: contentType, ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"), Hash: hashBody(body), FetchedAt: time.Now(),
	})
	return nil
}

func simplifyFetchError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
