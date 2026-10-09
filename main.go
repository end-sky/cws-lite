package main

// main.go: HTTP server, API handlers, cache and CRX download.
// All Chrome Web Store scraping/parsing lives in scrapers.go.

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

const (
	defaultAddr        = "127.0.0.1:8080"
	searchTimeout      = 9 * time.Second
	searchBodyLimit    = 6 << 20
	maxResults         = 24
	cacheLifetime      = 4 * time.Minute
	maxCachedSearches  = 128
	defaultProdVersion = "156.0.0.0"
)

var (
	bareIDRE           = regexp.MustCompile(`^[a-p]{32}$`)
	prodVersionRE      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	userAgentVersionRE = regexp.MustCompile(`(?i)(?:Chrome|Chromium)/([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)`)
	unsafeFilename     = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
)

type Listing struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Publisher   string `json:"publisher,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	URL         string `json:"url"`
	Kind        string `json:"kind"`
}

type SearchResponse struct {
	Query   string    `json:"query"`
	Kind    string    `json:"kind"`
	Source  string    `json:"source,omitempty"`
	Note    string    `json:"note,omitempty"`
	Results []Listing `json:"results"`
}

type cacheEntry struct {
	response SearchResponse
	expires  time.Time
	created  time.Time
}

type searchCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

func newSearchCache() *searchCache { return &searchCache{m: make(map[string]cacheEntry)} }

func (c *searchCache) get(key string) (SearchResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.m[key]
	if !ok {
		return SearchResponse{}, false
	}
	if time.Now().After(entry.expires) {
		delete(c.m, key)
		return SearchResponse{}, false
	}
	return entry.response, true
}

func (c *searchCache) put(key string, response SearchResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, entry := range c.m {
		if now.After(entry.expires) {
			delete(c.m, k)
		}
	}
	if len(c.m) >= maxCachedSearches {
		keys := make([]string, 0, len(c.m))
		for k := range c.m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return c.m[keys[i]].created.Before(c.m[keys[j]].created) })
		for i := 0; i < len(keys)-(maxCachedSearches-1); i++ {
			delete(c.m, keys[i])
		}
	}
	c.m[key] = cacheEntry{response: response, expires: now.Add(cacheLifetime), created: now}
}

var cache = newSearchCache()

func main() {
	webFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/search", handleSearch)
	mux.HandleFunc("GET /api/download", handleDownload)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("/", http.FileServer(http.FS(webFS)))

	addr := strings.TrimSpace(os.Getenv("ADDR"))
	if addr == "" {
		addr = defaultAddr
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("CWS Lite listening at http://%s", addr)
	log.Printf("Searches are proxied by this server; CRX packages are requested from Google's official update endpoint.")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if kind == "" {
		kind = "extensions"
	}
	if kind != "extensions" && kind != "themes" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be extensions or themes"})
		return
	}
	if len([]rune(q)) > 120 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Search is limited to 120 characters."})
		return
	}
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Enter a search term."})
		return
	}

	key := kind + "\x00" + strings.ToLower(q)
	if cached, ok := cache.get(key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout*3)
	defer cancel()
	response := searchListings(ctx, q, kind)
	if len(response.Results) > 0 { // never pin a failed/empty lookup in the cache
		cache.put(key, response)
	}
	writeJSON(w, http.StatusOK, response)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("id")))
	if !bareIDRE.MatchString(id) {
		http.Error(w, "Invalid Chrome Web Store extension ID.", http.StatusBadRequest)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if kind != "themes" {
		kind = "extensions"
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = id
	}
	filename := safeFilename(name) + ".crx"
	prodVersion := strings.TrimSpace(os.Getenv("CWS_PRODVERSION"))
	if prodVersion == "" {
		if match := userAgentVersionRE.FindStringSubmatch(r.UserAgent()); len(match) > 1 {
			prodVersion = match[1]
		} else {
			prodVersion = defaultProdVersion
		}
	}
	if !prodVersionRE.MatchString(prodVersion) {
		prodVersion = defaultProdVersion
	}

	query := url.Values{}
	query.Set("response", "redirect")
	query.Set("prodversion", prodVersion)
	query.Set("acceptformat", "crx3,crx2")
	query.Set("x", "id="+id+"&installsource=ondemand&uc")
	endpoint := "https://clients2.google.com/service/update2/crx?" + query.Encode()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		http.Error(w, "Could not prepare the download request.", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "application/x-chrome-extension,application/octet-stream,*/*")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Download failed while contacting Google's package endpoint. Check your connection, proxy, or try again later.", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Google's package endpoint returned HTTP %d. The listing may be removed, restricted, or temporarily unavailable.", resp.StatusCode), http.StatusBadGateway)
		return
	}

	reader := bufio.NewReader(resp.Body)
	magic, err := reader.Peek(4)
	if err != nil || !bytes.Equal(magic, []byte("Cr24")) {
		preview, _ := io.ReadAll(io.LimitReader(reader, 512))
		message := "Google did not return a valid CRX package. The extension may be unavailable for this browser version."
		if len(preview) > 0 && bytes.Contains(bytes.ToLower(preview), []byte("not found")) {
			message = "Google reports that this package was not found or is no longer available."
		}
		http.Error(w, message, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/x-chrome-extension")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-CWS-Kind", kind)
	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("streaming CRX %s failed: %v", id, err)
	}
}

func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "-")
	value = unsafeFilename.ReplaceAllString(value, "-")
	value = strings.Trim(value, ".-_ ")
	if value == "" {
		return "chrome-store-item"
	}
	return truncateRunes(value, 72)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("JSON response failed: %v", err)
	}
}
