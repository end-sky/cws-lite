package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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
	anchorRE           = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a\s*>`)
	attributeRE        = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	tagRE              = regexp.MustCompile(`(?s)<[^>]+>`)
	scriptStyleRE      = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>|<noscript\b[^>]*>.*?</noscript\s*>`)
	blockEndRE         = regexp.MustCompile(`(?i)</(?:div|p|h[1-6]|span|li|section|article|button|header|footer|strong|em|br)\s*>|<br\s*/?>`)
	spaceRE            = regexp.MustCompile(`[\t\x0b\f\r ]+`)
	storeIDRE          = regexp.MustCompile(`(?i)(?:/detail/[^/?#]*/|/detail/|[?&]id=)([a-p]{32})(?:[/?#&]|$)`)
	bareIDRE           = regexp.MustCompile(`^[a-p]{32}$`)
	prodVersionRE      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	userAgentVersionRE = regexp.MustCompile(`(?i)(?:Chrome|Chromium)/([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)`)
	storeHostRE        = regexp.MustCompile(`(?i)(?:^|\.)chromewebstore\.google\.com$`)
	legacyStoreRE      = regexp.MustCompile(`(?i)(?:^|\.)chrome\.google\.com$`)
	unsafeFilename     = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
)

type Listing struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
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
	cache.put(key, response)
	writeJSON(w, http.StatusOK, response)
}

func searchListings(ctx context.Context, query, kind string) SearchResponse {
	response := SearchResponse{Query: query, Kind: kind, Results: []Listing{}}
	storeQuery := query
	lowerQuery := strings.ToLower(query)
	if kind == "themes" && !strings.Contains(lowerQuery, "theme") {
		storeQuery += " theme"
	} else if kind == "extensions" && !strings.Contains(lowerQuery, "extension") {
		storeQuery += " extension"
	}

	storeURL := "https://chromewebstore.google.com/search/" + url.PathEscape(storeQuery) + "?hl=en-US"
	body, finalURL, err := fetchHTML(ctx, storeURL)
	if err == nil {
		response.Results = extractStoreResults(body, kind, finalURL)
		if len(response.Results) > 0 {
			response.Source = "Chrome Web Store"
			response.Note = "Fetched from the Web Store page. Listings are parsed from public page HTML, so a Store redesign may temporarily affect results."
			return response
		}
	}

	// Google increasingly relies on client-side rendering, so use an index-only fallback
	// rather than attempting to bypass a challenge, login, or anti-bot screen.
	fallbackQuery := "site:chromewebstore.google.com/detail " + query
	if kind == "themes" {
		fallbackQuery += " chrome theme"
	} else {
		fallbackQuery += " chrome extension"
	}
	fallbacks := []struct {
		name string
		url  string
	}{
		{name: "public search index (DuckDuckGo)", url: "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(fallbackQuery)},
		{name: "public search index (Bing)", url: "https://www.bing.com/search?q=" + url.QueryEscape(fallbackQuery)},
	}
	for _, fallback := range fallbacks {
		fallbackBody, _, fallbackErr := fetchHTML(ctx, fallback.url)
		if fallbackErr != nil {
			continue
		}
		response.Results = extractStoreResults(fallbackBody, kind, fallback.url)
		if len(response.Results) > 0 {
			response.Source = fallback.name
			response.Note = "The Web Store page did not expose readable listings, so these links came from a public search index. Open a listing to verify its details before downloading."
			return response
		}
	}

	response.Note = "No listings could be parsed. The Web Store may be using a new page layout or temporarily rate-limiting automated requests. Try a shorter query, wait a little, or open the Web Store directly."
	return response
}

func fetchHTML(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	client := &http.Client{Timeout: searchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Request.URL.String(), fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, searchBodyLimit))
	if err != nil {
		return nil, resp.Request.URL.String(), err
	}
	return body, resp.Request.URL.String(), nil
}

func extractStoreResults(body []byte, kind, pageURL string) []Listing {
	s := string(body)
	matches := anchorRE.FindAllStringSubmatchIndex(s, -1)
	results := make([]Listing, 0, 12)
	seen := make(map[string]int)

	for _, m := range matches {
		attrsText := s[m[2]:m[3]]
		inner := s[m[4]:m[5]]
		attrs := parseAttributes(attrsText)
		href := html.UnescapeString(strings.TrimSpace(attrs["href"]))
		if href == "" {
			continue
		}
		resolved := normalizeStoreHref(href, pageURL)
		id := extractIDFromURL(resolved)
		if id == "" {
			continue
		}
		listingURL := "https://chromewebstore.google.com/detail/" + id
		if parsed, err := url.Parse(resolved); err == nil && strings.Contains(parsed.Hostname(), "chromewebstore.google.com") && strings.Contains(parsed.Path, "/detail/") {
			listingURL = resolved
		} else if parsed, err := url.Parse(resolved); err == nil && parsed.Hostname() == "chrome.google.com" && strings.Contains(parsed.Path, "/webstore/detail/") {
			listingURL = resolved
		}

		lines := visibleLines(inner)
		name := firstNonEmpty(attrs["aria-label"], attrs["title"])
		if name == "" {
			name = chooseName(lines)
		}
		if name == "" {
			name = "Chrome Web Store listing"
		}
		name = trimListingName(name)
		description := chooseDescription(lines, name)
		if description == "" {
			description = nearbySnippet(s, m[1], 1900)
		}
		if len([]rune(description)) > 360 {
			description = string([]rune(description)[:357]) + "…"
		}
		icon := extractIcon(inner)
		if icon != "" && !isSafeRemoteImage(icon) {
			icon = ""
		}

		candidate := Listing{ID: id, Name: name, Description: description, Icon: icon, URL: listingURL, Kind: kind}
		if existing, ok := seen[id]; ok {
			// Prefer a real description/icon from whichever anchor has more card data.
			old := results[existing]
			if len(candidate.Description) > len(old.Description) {
				old.Description = candidate.Description
			}
			if old.Icon == "" {
				old.Icon = candidate.Icon
			}
			if old.Name == "Chrome Web Store listing" && candidate.Name != old.Name {
				old.Name = candidate.Name
			}
			results[existing] = old
			continue
		}
		seen[id] = len(results)
		results = append(results, candidate)
		if len(results) >= maxResults {
			break
		}
	}
	return results
}

func parseAttributes(raw string) map[string]string {
	attrs := make(map[string]string)
	for _, match := range attributeRE.FindAllStringSubmatch(raw, -1) {
		value := match[2]
		if value == "" {
			value = match[3]
		}
		if value == "" {
			value = match[4]
		}
		attrs[strings.ToLower(match[1])] = html.UnescapeString(value)
	}
	return attrs
}

func normalizeStoreHref(href, pageURL string) string {
	unescaped := html.UnescapeString(href)
	parsed, err := url.Parse(unescaped)
	if err != nil {
		return ""
	}
	if (parsed.Host == "duckduckgo.com" || strings.HasSuffix(parsed.Host, ".duckduckgo.com")) && parsed.Path == "/l/" {
		if wrapped := parsed.Query().Get("uddg"); wrapped != "" {
			unescaped = wrapped
			parsed, err = url.Parse(unescaped)
			if err != nil {
				return ""
			}
		}
	}
	if strings.HasSuffix(strings.ToLower(parsed.Hostname()), "bing.com") && parsed.Path == "/ck/a" {
		if wrapped := parsed.Query().Get("u"); strings.HasPrefix(wrapped, "a1") {
			token := strings.TrimPrefix(wrapped, "a1")
			var decoded []byte
			for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
				if candidate, decodeErr := encoding.DecodeString(token); decodeErr == nil && strings.HasPrefix(string(candidate), "http") {
					decoded = candidate
					break
				}
			}
			if len(decoded) > 0 {
				parsed, err = url.Parse(string(decoded))
				if err != nil {
					return ""
				}
			}
		}
	}
	if !parsed.IsAbs() {
		base, baseErr := url.Parse(pageURL)
		if baseErr != nil {
			return ""
		}
		parsed = base.ResolveReference(parsed)
	}
	host := strings.ToLower(parsed.Hostname())
	if !(storeHostRE.MatchString(host) || (legacyStoreRE.MatchString(host) && strings.Contains(parsed.Path, "/webstore/"))) {
		return ""
	}
	return parsed.String()
}

func extractIDFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if !(storeHostRE.MatchString(host) || legacyStoreRE.MatchString(host)) {
		return ""
	}
	matches := storeIDRE.FindStringSubmatch(parsed.Path)
	if len(matches) < 2 {
		matches = storeIDRE.FindStringSubmatch(parsed.RawQuery)
	}
	if len(matches) >= 2 && bareIDRE.MatchString(strings.ToLower(matches[1])) {
		return strings.ToLower(matches[1])
	}
	return ""
}

func visibleLines(fragment string) []string {
	fragment = scriptStyleRE.ReplaceAllString(fragment, " ")
	fragment = blockEndRE.ReplaceAllString(fragment, "\n")
	fragment = tagRE.ReplaceAllString(fragment, " ")
	fragment = html.UnescapeString(fragment)
	fragment = strings.ReplaceAll(fragment, "\u200b", "")
	fragment = strings.ReplaceAll(fragment, "\u00a0", " ")
	lines := strings.Split(fragment, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(spaceRE.ReplaceAllString(line, " "))
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func chooseName(lines []string) string {
	for _, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		if lower == "featured" || lower == "extensions" || lower == "themes" || lower == "overview" || lower == "chrome web store" {
			continue
		}
		if len([]rune(line)) > 140 {
			continue
		}
		if strings.Contains(lower, "users") && len(strings.Fields(line)) < 4 {
			continue
		}
		if strings.Contains(lower, "rating") && len(strings.Fields(line)) < 5 {
			continue
		}
		return line
	}
	return ""
}

func chooseDescription(lines []string, name string) string {
	parts := make([]string, 0, 3)
	for _, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), strings.TrimSpace(name)) {
			continue
		}
		lower := strings.ToLower(line)
		if lower == "featured" || lower == "extensions" || lower == "themes" || lower == "overview" || lower == "chrome web store" {
			continue
		}
		if strings.Contains(lower, "add to chrome") || strings.Contains(lower, "add to chromium") {
			continue
		}
		if len([]rune(line)) > 260 {
			line = string([]rune(line)[:257]) + "…"
		}
		parts = append(parts, line)
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func trimListingName(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "|·•-–—"))
	for _, sep := range []string{"\n", " | ", " · ", " — ", " – "} {
		if i := strings.Index(s, sep); i > 0 && i < 140 {
			s = s[:i]
		}
	}
	if len([]rune(s)) > 100 {
		s = string([]rune(s)[:97]) + "…"
	}
	return s
}

func nearbySnippet(body string, pos, radius int) string {
	start := pos
	if start < 0 {
		start = 0
	}
	end := pos + radius
	if end > len(body) {
		end = len(body)
	}
	if end < start {
		return ""
	}
	contextHTML := body[start:end]
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?is)<(?:div|span)[^>]*class=["'][^"']*(?:result__snippet|b_caption|snippet|description)[^"']*["'][^>]*>(.*?)</(?:div|span)\s*>`),
		regexp.MustCompile(`(?is)<meta[^>]+name=["']description["'][^>]+content=["']([^"']+)`),
	}
	for _, re := range patterns {
		match := re.FindStringSubmatch(contextHTML)
		if len(match) > 1 {
			lines := visibleLines(match[1])
			if len(lines) > 0 {
				return truncateRunes(strings.Join(lines, " "), 360)
			}
		}
	}
	return ""
}

func extractIcon(inner string) string {
	imgRE := regexp.MustCompile(`(?is)<img\b([^>]*)>`)
	for _, match := range imgRE.FindAllStringSubmatch(inner, -1) {
		attrs := parseAttributes(match[1])
		for _, key := range []string{"src", "data-src", "data-lazy-src"} {
			if value := strings.TrimSpace(attrs[key]); value != "" {
				if strings.HasPrefix(value, "//") {
					return "https:" + value
				}
				if strings.HasPrefix(value, "https://") {
					return value
				}
			}
		}
	}
	return ""
}

func isSafeRemoteImage(s string) bool {
	parsed, err := url.Parse(s)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func truncateRunes(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
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
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")
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
