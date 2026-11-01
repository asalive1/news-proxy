package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

//go:embed defaults/sources.json
var defaultSourcesJSON []byte

const (
	defaultPort            = 7999
	defaultRefreshMinutes  = 10
	maxRefreshMinutes      = 1440
	settingsFileName       = "settings.json"
	sourcesFileName        = "sources.json"
	maxSourceNameLength    = 100
	maxCategoriesPerSource = 50
)

// Settings are the service options. Port and BindAddress need a restart; everything else applies live.
type Settings struct {
	Port                   int    `json:"port"`
	BindAddress            string `json:"bindAddress"`
	RefreshIntervalMinutes int    `json:"refreshIntervalMinutes"`
	AllowPrivateFeeds      bool   `json:"allowPrivateFeeds"`
	RelayDisabled          bool   `json:"relayDisabled,omitempty"`
	AdminPasswordHash      string `json:"adminPasswordHash,omitempty"`
}

func defaultSettings() Settings {
	return Settings{Port: defaultPort, RefreshIntervalMinutes: defaultRefreshMinutes}
}

func (s *Settings) sanitize() {
	if s.Port < 1 || s.Port > 65535 {
		s.Port = defaultPort
	}
	if s.RefreshIntervalMinutes < 1 {
		s.RefreshIntervalMinutes = defaultRefreshMinutes
	}
	if s.RefreshIntervalMinutes > maxRefreshMinutes {
		s.RefreshIntervalMinutes = maxRefreshMinutes
	}
	s.BindAddress = strings.TrimSpace(s.BindAddress)
}

// Source is one news feed. ProxyPath is derived, never stored.
type Source struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Type       string   `json:"type,omitempty"`
	Group      string   `json:"group,omitempty"`
	Categories []string `json:"categories"`
	Aliases    []string `json:"aliases,omitempty"`
}

type sourcesFile struct {
	Version int      `json:"version"`
	Sources []Source `json:"sources"`
}

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	slugPattern     = regexp.MustCompile(`[^a-z0-9]+`)
	reservedSources = map[string]bool{"health": true, "local": true, "relay": true, "config": true, "feed": true, "sources.json": true, "favicon.ico": true}
)

func slugify(text string) string {
	slug := strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(text), "-"), "-")
	if len(slug) > 64 {
		slug = strings.Trim(slug[:64], "-")
	}
	if slug == "" {
		slug = "source"
	}
	return slug
}

func feedPath(id string) string { return "/feed/" + id }

// normalizeSource trims and validates user input. Returns the cleaned source or a user-facing error.
func normalizeSource(in Source) (Source, error) {
	out := Source{
		ID:    strings.ToLower(strings.TrimSpace(in.ID)),
		Name:  strings.TrimSpace(in.Name),
		URL:   strings.TrimSpace(in.URL),
		Group: strings.TrimSpace(in.Group),
		Type:  strings.ToUpper(strings.TrimSpace(in.Type)),
	}
	if out.Name == "" {
		return out, errors.New("A friendly name is required.")
	}
	if len(out.Name) > maxSourceNameLength {
		return out, fmt.Errorf("The name must be %d characters or fewer.", maxSourceNameLength)
	}
	parsed, err := url.Parse(out.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return out, errors.New("Enter a feed URL starting with http:// or https://.")
	}
	if out.Type != "RSS" && out.Type != "ATOM" && out.Type != "XML" {
		out.Type = "RSS"
	}
	seen := map[string]bool{}
	out.Categories = []string{}
	for _, category := range in.Categories {
		category = strings.TrimSpace(category)
		key := strings.ToLower(category)
		if category == "" || seen[key] || len(out.Categories) >= maxCategoriesPerSource {
			continue
		}
		seen[key] = true
		out.Categories = append(out.Categories, category)
	}
	for _, alias := range in.Aliases {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if idPattern.MatchString(alias) && !reservedSources[alias] {
			out.Aliases = append(out.Aliases, alias)
		}
	}
	if out.ID != "" && !idPattern.MatchString(out.ID) {
		return out, errors.New("The id may contain only lowercase letters, numbers, - and _.")
	}
	return out, nil
}

func readJSONFile(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, target)
}

// writeJSONFile writes atomically so a crash never leaves a half-written config.
func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func envInt(name string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	return value, err == nil
}

// Password handling: salted, iterated SHA-256 stored as "v1$salt$hash".
const passwordRounds = 100_000

func derivePassword(password string, salt []byte) []byte {
	sum := sha256.Sum256(append(append([]byte{}, salt...), password...))
	for i := 0; i < passwordRounds; i++ {
		buf := make([]byte, 0, len(sum)+len(salt))
		buf = append(buf, sum[:]...)
		buf = append(buf, salt...)
		sum = sha256.Sum256(buf)
	}
	return sum[:]
}

func hashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	return "v1$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(derivePassword(password, salt))
}

func checkPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	salt, err1 := hex.DecodeString(parts[1])
	want, err2 := hex.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return false
	}
	return subtle.ConstantTimeCompare(derivePassword(password, salt), want) == 1
}
