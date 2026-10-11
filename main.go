// CWS Lite - a small local front end for the Chrome Web Store and Firefox Add-ons.
//
//	main.go     HTTP server, routing, search cache
//	common/     shared types and helpers
//	google/     Chrome Web Store scraper + CRX download
//	firefox/    Firefox Add-ons (AMO API) search + XPI download
//	web/        embedded front end (index.html, style.css, scripts.go)
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cws-lite/common"
	"cws-lite/firefox"
	"cws-lite/google"
	"cws-lite/web"
)

// Version is the release version, also reported by /api/health.
const Version = "1.0.0"

//go:embed web/index.html web/style.css
var webFiles embed.FS

const (
	defaultAddr       = "127.0.0.1:8080"
	cacheLifetime     = 4 * time.Minute
	maxCachedSearches = 128
	maxQueryRunes     = 120
)

// stores maps the ?store= value to its back end.
var stores = map[string]struct {
	search   func(ctx context.Context, query, kind string) common.SearchResponse
	download http.HandlerFunc
}{
	"google":  {google.Search, google.Download},
	"firefox": {firefox.Search, firefox.Download},
}

const defaultStore = "google"

type cacheEntry struct {
	response common.SearchResponse
	expires  time.Time
	created  time.Time
}

type searchCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

func newSearchCache() *searchCache { return &searchCache{m: make(map[string]cacheEntry)} }

func (c *searchCache) get(key string) (common.SearchResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.m[key]
	if !ok {
		return common.SearchResponse{}, false
	}
	if time.Now().After(entry.expires) {
		delete(c.m, key)
		return common.SearchResponse{}, false
	}
	return entry.response, true
}

func (c *searchCache) put(key string, response common.SearchResponse) {
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
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
	})
	mux.HandleFunc("GET /scripts.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = io.WriteString(w, web.Script)
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
	log.Printf("CWS Lite %s listening at http://%s", Version, addr)
	log.Printf("Searches are proxied by this server; packages come from Google's and Mozilla's official endpoints.")
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

func storeParam(r *http.Request) string {
	store := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("store")))
	if store == "" {
		return defaultStore
	}
	return store
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if kind == "" {
		kind = common.KindExtensions
	}
	store := storeParam(r)
	backend, ok := stores[store]
	switch {
	case !ok:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "store must be google or firefox"})
		return
	case kind != common.KindExtensions && kind != common.KindThemes:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be extensions or themes"})
		return
	case len([]rune(q)) > maxQueryRunes:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Search is limited to 120 characters."})
		return
	case q == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Enter a search term."})
		return
	}

	key := store + "\x00" + kind + "\x00" + strings.ToLower(q)
	if cached, ok := cache.get(key); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), common.SearchTimeout*3)
	defer cancel()
	response := backend.search(ctx, q, kind)
	if len(response.Results) > 0 { // never pin a failed/empty lookup in the cache
		cache.put(key, response)
	}
	writeJSON(w, http.StatusOK, response)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	backend, ok := stores[storeParam(r)]
	if !ok {
		http.Error(w, "store must be google or firefox", http.StatusBadRequest)
		return
	}
	backend.download(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("JSON response failed: %v", err)
	}
}
