package google

// scrapers.go contains everything that talks to (or parses pages from) the
// Chrome Web Store and the public search-index fallbacks. main.go only knows
// about Search().
//
// Extraction is layered so that a single layout change doesn't blank out the
// results:
//
//  1. Store pages: embedded AF_initDataCallback JSON blobs (the Store is a
//     client-rendered app and its data lives there), then plain <a> cards.
//  2. Search-index fallbacks (DuckDuckGo / Bing): title + snippet anchors.
//  3. Enrichment: every listing that is missing a title, description or
//     publisher is completed from its own detail page (JSON-LD, Open Graph
//     tags, "Offered by" text). Search pages never carry the publisher.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"troller/common"
)

const (
	placeholderName   = "Chrome Web Store listing"
	detailTimeout     = 6 * time.Second
	enrichBudget      = 10 * time.Second
	enrichConcurrency = 6
	detailLifetime    = 30 * time.Minute
	maxCachedDetails  = 512
	maxEmbeddedBlob   = 4 << 20
)

var (
	anchorRE      = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a\s*>`)
	attributeRE   = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	tagRE         = regexp.MustCompile(`(?s)<[^>]+>`)
	scriptStyleRE = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>|<noscript\b[^>]*>.*?</noscript\s*>`)
	blockEndRE    = regexp.MustCompile(`(?i)</(?:div|p|h[1-6]|span|li|section|article|button|header|footer|strong|em|br|a|dd|dt|td|th)\s*>|<br\s*/?>|<(?:div|p|h[1-6]|li|section|article|header|footer)\b[^>]*>`)
	spaceRE       = regexp.MustCompile(`[\t\x0b\f\r ]+`)
	bareIDRE      = regexp.MustCompile(`^[a-p]{32}$`)
	storeIDRE     = regexp.MustCompile(`(?i)(?:/detail/[^/?#]*/|/detail/|[?&]id=)([a-p]{32})(?:[/?#&]|$)`)
	storeHostRE   = regexp.MustCompile(`(?i)(?:^|\.)chromewebstore\.google\.com$`)
	legacyStoreRE = regexp.MustCompile(`(?i)(?:^|\.)chrome\.google\.com$`)

	headingOpenRE  = regexp.MustCompile(`<h[1-6][\s>]`)
	headingRE      = regexp.MustCompile(`(?is)<h[1-6]\b[^>]*>(.*?)</h[1-6]\s*>`)
	imgRE          = regexp.MustCompile(`(?is)<img\b([^>]*)>`)
	metaRE         = regexp.MustCompile(`(?is)<meta\b([^>]*)>`)
	titleRE        = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
	jsonLDRE       = regexp.MustCompile(`(?is)<script\b[^>]*type\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script\s*>`)
	storeSuffixRE  = regexp.MustCompile(`(?i)\s*[-–—|·:]\s*Chrome Web Store\s*$`)
	byLineRE       = regexp.MustCompile(`(?i)^(?:offered|published|developed|provided)?\s*by\s*:?\s*(.+)$`)
	offeredByRE    = regexp.MustCompile(`(?i)^offered by\s*:?\s*(.*)$`)
	ratingLineRE   = regexp.MustCompile(`(?i)^\(?[0-9][0-9.,]*\s?[km]?\+?\)?(\s*(ratings?|users?|reviews?|stars?|out of 5.*))?$`)
	ratedLineRE    = regexp.MustCompile(`(?i)^(rated\s+)?[0-9.]+\s*(\(.*\)|out of 5.*|stars?.*)$`)
	snippetBlockRE = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<(?:div|span|p)[^>]*class=["'][^"']*(?:result__snippet|b_caption|b_lineclamp|snippet|description)[^"']*["'][^>]*>(.*?)</(?:div|span|p)\s*>`),
		regexp.MustCompile(`(?is)<meta[^>]+name=["']description["'][^>]+content=["']([^"']+)`),
	}
	noiseLines = map[string]bool{
		"featured": true, "extensions": true, "extension": true, "themes": true, "theme": true,
		"overview": true, "chrome web store": true, "add to chrome": true, "add to chromium": true,
		"remove from chrome": true, "established publisher": true, "editors' pick": true,
		"editor's pick": true, "editors’ pick": true, "recommended": true, "install": true,
	}
)

// ---------------------------------------------------------------------------
// Searching
// ---------------------------------------------------------------------------

// Search looks the query up on the Chrome Web Store (falling back to public search indexes).
func Search(ctx context.Context, query, kind string) common.SearchResponse {
	response := common.SearchResponse{Query: query, Store: "google", Kind: kind, Results: []common.Listing{}}
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
			response.Results = enrichListings(ctx, response.Results)
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
			response.Results = enrichListings(ctx, response.Results)
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
	req.Header.Set("User-Agent", common.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	client := &http.Client{Timeout: common.SearchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Request.URL.String(), fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, common.SearchBodyLimit))
	if err != nil {
		return nil, resp.Request.URL.String(), err
	}
	return body, resp.Request.URL.String(), nil
}

// ---------------------------------------------------------------------------
// Result extraction
// ---------------------------------------------------------------------------

// extractStoreResults parses a Store search page or a search-index results page.
func extractStoreResults(body []byte, kind, pageURL string) []common.Listing {
	s := string(body)
	var results []common.Listing
	index := make(map[string]int)

	add := func(candidate common.Listing) bool {
		if existing, ok := index[candidate.ID]; ok {
			results[existing] = mergeListings(results[existing], candidate)
			return false
		}
		if len(results) >= common.MaxResults {
			return false
		}
		index[candidate.ID] = len(results)
		results = append(results, candidate)
		return true
	}

	// 1. Data blobs embedded in the page (only the Store itself has these).
	for _, candidate := range extractEmbeddedListings(s, kind) {
		add(candidate)
	}
	// 2. Anchor cards / search-engine result links.
	for _, candidate := range extractAnchorListings(s, kind, pageURL) {
		add(candidate)
	}

	for i := range results {
		if results[i].Name == "" {
			results[i].Name = placeholderName
		}
	}
	return results
}

// mergeListings fills the gaps of a with whatever b knows.
func mergeListings(a, b common.Listing) common.Listing {
	if a.Name == "" || a.Name == placeholderName {
		a.Name = b.Name
	}
	if len(b.Description) > len(a.Description) {
		a.Description = b.Description
	}
	if a.Publisher == "" {
		a.Publisher = b.Publisher
	}
	if a.Icon == "" {
		a.Icon = b.Icon
	}
	return a
}

func newListing(id, kind string) common.Listing {
	return common.Listing{ID: id, URL: "https://chromewebstore.google.com/detail/" + id, Kind: kind}
}

func extractAnchorListings(s, kind, pageURL string) []common.Listing {
	matches := anchorRE.FindAllStringSubmatchIndex(s, -1)
	out := make([]common.Listing, 0, 16)

	for _, m := range matches {
		attrs := parseAttributes(s[m[2]:m[3]])
		inner := s[m[4]:m[5]]
		href := html.UnescapeString(strings.TrimSpace(attrs["href"]))
		if href == "" {
			continue
		}
		resolved := normalizeStoreHref(href, pageURL)
		id := extractIDFromURL(resolved)
		if id == "" {
			continue
		}
		listing := newListing(id, kind)
		if parsed, err := url.Parse(resolved); err == nil && strings.Contains(parsed.Path, "/detail/") {
			listing.URL = resolved
			if parsed.Hostname() == "chromewebstore.google.com" {
				// Drop tracking parameters; keep the canonical listing URL.
				parsed.RawQuery = ""
				parsed.Fragment = ""
				listing.URL = parsed.String()
			}
		}

		class := strings.ToLower(attrs["class"])
		lines := visibleLines(inner)

		switch {
		case strings.Contains(class, "result__snippet"):
			// DuckDuckGo snippet link: pure description.
			listing.Description = common.TruncateRunes(strings.Join(lines, " "), 360)
		case strings.Contains(class, "result__a") || insideHeading(s, m[0]):
			// Search-engine title link ("Name - Chrome Web Store").
			listing.Name = trimListingName(strings.Join(lines, " "))
			if listing.Description == "" {
				listing.Description = nearbySnippet(s, m[1], 1900)
			}
		default:
			// Store card.
			listing.Name = cardName(inner, attrs, lines)
			listing.Publisher, lines = splitPublisher(lines)
			listing.Description = chooseDescription(lines, listing.Name)
			if listing.Description == "" {
				listing.Description = nearbySnippet(s, m[1], 1900)
			}
		}

		listing.Description = common.TruncateRunes(listing.Description, 360)
		if icon := extractIcon(inner); icon != "" && isSafeRemoteImage(icon) {
			listing.Icon = icon
		}
		out = append(out, listing)
	}
	return out
}

// insideHeading reports whether the anchor starting at pos sits directly inside an <h2>/<h3>
// (Bing wraps each result title that way).
func insideHeading(s string, pos int) bool {
	start := pos - 80
	if start < 0 {
		start = 0
	}
	window := strings.ToLower(s[start:pos])
	opens := headingOpenRE.FindAllStringIndex(window, -1)
	if len(opens) == 0 {
		return false
	}
	closed := strings.LastIndex(window, "</h")
	return opens[len(opens)-1][0] > closed
}

func cardName(inner string, attrs map[string]string, lines []string) string {
	if m := headingRE.FindStringSubmatch(inner); len(m) > 1 {
		if hl := visibleLines(m[1]); len(hl) > 0 {
			if name := trimListingName(hl[0]); name != "" {
				return name
			}
		}
	}
	if name := common.FirstNonEmpty(attrs["aria-label"], attrs["title"]); name != "" {
		return trimListingName(name)
	}
	if img := firstImgAlt(inner); img != "" && !strings.Contains(strings.ToLower(img), "icon") {
		return trimListingName(img)
	}
	return trimListingName(chooseName(lines))
}

func firstImgAlt(inner string) string {
	for _, match := range imgRE.FindAllStringSubmatch(inner, -1) {
		if alt := strings.TrimSpace(parseAttributes(match[1])["alt"]); alt != "" {
			return alt
		}
	}
	return ""
}

// splitPublisher pulls an "Offered by X" / "By X" line out of a card.
func splitPublisher(lines []string) (string, []string) {
	rest := make([]string, 0, len(lines))
	publisher := ""
	for _, line := range lines {
		if publisher == "" {
			if m := byLineRE.FindStringSubmatch(line); len(m) > 1 && len([]rune(m[1])) <= 80 && len(strings.Fields(line)) <= 8 {
				publisher = strings.TrimSpace(m[1])
				continue
			}
		}
		rest = append(rest, line)
	}
	return publisher, rest
}

// ---------------------------------------------------------------------------
// Embedded AF_initDataCallback data
// ---------------------------------------------------------------------------

func extractEmbeddedListings(s, kind string) []common.Listing {
	var out []common.Listing
	seen := make(map[string]bool)
	for _, blob := range embeddedBlobs(s) {
		walkForListings(blob, 0, func(l common.Listing) {
			if seen[l.ID] {
				return
			}
			seen[l.ID] = true
			l.Kind = kind
			out = append(out, l)
		})
	}
	return out
}

// embeddedBlobs returns the decoded `data:` payload of each AF_initDataCallback({...}) call.
func embeddedBlobs(s string) []any {
	const marker = "AF_initDataCallback("
	var blobs []any
	pos := 0
	for {
		i := strings.Index(s[pos:], marker)
		if i < 0 {
			return blobs
		}
		start := pos + i + len(marker)
		pos = start
		d := strings.Index(s[start:], "data:")
		if d < 0 {
			return blobs
		}
		if d > 400 { // belongs to some other call
			continue
		}
		p := start + d + len("data:")
		br := strings.IndexByte(s[p:], '[')
		if br < 0 || br > 40 {
			continue
		}
		raw := balancedArray(s[p+br:])
		if raw == "" {
			continue
		}
		var v any
		if json.Unmarshal([]byte(raw), &v) == nil {
			blobs = append(blobs, v)
		}
	}
}

// balancedArray returns the JSON array literal at the start of s.
func balancedArray(s string) string {
	depth, inString, escaped := 0, false, false
	limit := len(s)
	if limit > maxEmbeddedBlob {
		limit = maxEmbeddedBlob
	}
	for i := 0; i < limit; i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}

func walkForListings(node any, depth int, emit func(common.Listing)) {
	if depth > 14 {
		return
	}
	arr, ok := node.([]any)
	if !ok {
		return
	}
	if l, ok := listingFromArray(arr); ok {
		emit(l)
		return
	}
	for _, child := range arr {
		walkForListings(child, depth+1, emit)
	}
}

// listingFromArray recognises the Store's positional listing record:
// [id, ?, iconURL, name, rating, ratingCount, ..., description, ...].
// Positions are only a first guess; anything else is found heuristically, and
// detail-page enrichment corrects what is still wrong.
func listingFromArray(arr []any) (common.Listing, bool) {
	if len(arr) < 4 {
		return common.Listing{}, false
	}
	id, ok := arr[0].(string)
	if !ok || !bareIDRE.MatchString(id) {
		return common.Listing{}, false
	}
	var texts []string
	var icon string
	collectStrings(arr[1:], 0, func(v string) {
		v = strings.TrimSpace(v)
		switch {
		case v == "":
		case strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "//"):
			if icon == "" && isImageHost(v) {
				icon = strings.TrimPrefix(v, "//")
				if !strings.HasPrefix(icon, "http") {
					icon = "https://" + icon
				}
			}
		case bareIDRE.MatchString(v), strings.HasPrefix(v, "/"):
		default:
			texts = append(texts, v)
		}
	})

	l := newListing(id, "")
	if s, ok := arr[3].(string); ok && s != "" && !strings.HasPrefix(s, "http") && len([]rune(s)) <= 100 {
		l.Name = trimListingName(html.UnescapeString(s))
	}
	if l.Name == "" {
		for _, t := range texts {
			if len([]rune(t)) <= 100 && !strings.Contains(t, "\n") && !isNoiseLine(t) {
				l.Name = trimListingName(html.UnescapeString(t))
				break
			}
		}
	}
	best := ""
	for _, t := range texts {
		if t != l.Name && len([]rune(t)) >= 30 && len(t) > len(best) {
			best = t
		}
	}
	l.Description = common.TruncateRunes(strings.Join(visibleLines(best), " "), 360)
	if icon != "" && isSafeRemoteImage(icon) {
		l.Icon = icon
	}
	if l.Name == "" || (l.Description == "" && l.Icon == "") {
		return common.Listing{}, false
	}
	return l, true
}

func collectStrings(v any, depth int, fn func(string)) {
	if depth > 3 {
		return
	}
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, c := range t {
			collectStrings(c, depth+1, fn)
		}
	}
}

func isImageHost(raw string) bool {
	u, err := url.Parse(strings.TrimPrefix(raw, "//"))
	if err != nil {
		return false
	}
	if u.Host == "" {
		u, err = url.Parse("https://" + strings.TrimPrefix(raw, "//"))
		if err != nil {
			return false
		}
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasSuffix(h, "googleusercontent.com") || strings.HasSuffix(h, "gstatic.com") || strings.HasSuffix(h, "ggpht.com")
}

// ---------------------------------------------------------------------------
// Detail-page enrichment (publisher, title, description)
// ---------------------------------------------------------------------------

type detailInfo struct {
	Name        string
	Publisher   string
	Description string
	Icon        string
}

type detailEntry struct {
	info    detailInfo
	expires time.Time
}

var (
	detailMu    sync.Mutex
	detailStore = make(map[string]detailEntry)
)

func cachedDetail(id string) (detailInfo, bool) {
	detailMu.Lock()
	defer detailMu.Unlock()
	e, ok := detailStore[id]
	if !ok || time.Now().After(e.expires) {
		delete(detailStore, id)
		return detailInfo{}, false
	}
	return e.info, true
}

func storeDetail(id string, info detailInfo) {
	detailMu.Lock()
	defer detailMu.Unlock()
	now := time.Now()
	if len(detailStore) >= maxCachedDetails {
		for k, e := range detailStore {
			if now.After(e.expires) {
				delete(detailStore, k)
			}
		}
		if len(detailStore) >= maxCachedDetails {
			detailStore = make(map[string]detailEntry)
		}
	}
	detailStore[id] = detailEntry{info: info, expires: now.Add(detailLifetime)}
}

// enrichListings completes missing fields from each listing's detail page, in
// parallel and within a fixed time budget. Failures are silent: the listing
// just keeps whatever the search page gave us.
func enrichListings(ctx context.Context, listings []common.Listing) []common.Listing {
	ctx, cancel := context.WithTimeout(ctx, enrichBudget)
	defer cancel()

	sem := make(chan struct{}, enrichConcurrency)
	var wg sync.WaitGroup
	for i := range listings {
		wg.Add(1)
		sem <- struct{}{}
		go func(l *common.Listing) {
			defer wg.Done()
			defer func() { <-sem }()
			info, ok := cachedDetail(l.ID)
			if !ok {
				fetched, err := fetchDetail(ctx, l.ID)
				if err != nil {
					return
				}
				info = fetched
				storeDetail(l.ID, info)
			}
			applyDetail(l, info)
		}(&listings[i])
	}
	wg.Wait()
	return listings
}

// applyDetail: the listing's own page is authoritative for text fields.
func applyDetail(l *common.Listing, d detailInfo) {
	if d.Name != "" {
		l.Name = d.Name
	}
	if d.Description != "" {
		l.Description = common.TruncateRunes(d.Description, 360)
	}
	if d.Publisher != "" {
		l.Publisher = d.Publisher
	}
	if l.Icon == "" && d.Icon != "" && isSafeRemoteImage(d.Icon) {
		l.Icon = d.Icon
	}
}

func fetchDetail(ctx context.Context, id string) (detailInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, detailTimeout)
	defer cancel()
	body, _, err := fetchHTML(dctx, "https://chromewebstore.google.com/detail/"+id+"?hl=en-US")
	if err != nil {
		return detailInfo{}, err
	}
	return parseDetailPage(body), nil
}

// parseDetailPage reads a listing page using several independent sources;
// earlier sources win, later ones only fill what is still empty.
func parseDetailPage(body []byte) detailInfo {
	s := string(body)
	var d detailInfo

	// 1. JSON-LD
	for _, m := range jsonLDRE.FindAllStringSubmatch(s, -1) {
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v) != nil {
			continue
		}
		mergeJSONLD(&d, v, 0)
	}

	// 2. Open Graph / standard meta tags
	meta := map[string]string{}
	for _, m := range metaRE.FindAllStringSubmatch(s, -1) {
		a := parseAttributes(m[1])
		key := strings.ToLower(common.FirstNonEmpty(a["property"], a["name"], a["itemprop"]))
		if key != "" && a["content"] != "" {
			if _, dup := meta[key]; !dup {
				meta[key] = strings.TrimSpace(a["content"])
			}
		}
	}
	if d.Name == "" {
		d.Name = cleanTitle(common.FirstNonEmpty(meta["og:title"], meta["twitter:title"], meta["name"]))
	}
	if d.Description == "" {
		d.Description = cleanText(common.FirstNonEmpty(meta["og:description"], meta["description"], meta["twitter:description"]))
	}
	if d.Publisher == "" {
		d.Publisher = cleanText(meta["author"])
	}
	if d.Icon == "" {
		d.Icon = common.FirstNonEmpty(meta["og:image"], meta["image"])
	}
	if d.Name == "" {
		if m := titleRE.FindStringSubmatch(s); len(m) > 1 {
			d.Name = cleanTitle(m[1])
		}
	}

	// 3. "Offered by" text on the rendered page
	if d.Publisher == "" {
		lines := visibleLines(s)
		for i, line := range lines {
			if m := offeredByRE.FindStringSubmatch(line); len(m) > 1 {
				candidate := strings.TrimSpace(m[1])
				if candidate == "" && i+1 < len(lines) {
					candidate = lines[i+1]
				}
				if candidate != "" && len([]rune(candidate)) <= 80 {
					d.Publisher = candidate
					break
				}
			}
		}
	}

	d.Publisher = trimPublisher(d.Publisher)
	if strings.EqualFold(d.Name, "chrome web store") {
		d.Name = ""
	}
	return d
}

func mergeJSONLD(d *detailInfo, v any, depth int) {
	if depth > 6 {
		return
	}
	switch t := v.(type) {
	case []any:
		for _, c := range t {
			mergeJSONLD(d, c, depth+1)
		}
	case map[string]any:
		if g, ok := t["@graph"]; ok {
			mergeJSONLD(d, g, depth+1)
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		if name == "" && desc == "" {
			return
		}
		if d.Name == "" {
			d.Name = cleanTitle(name)
		}
		if d.Description == "" {
			d.Description = cleanText(desc)
		}
		if d.Publisher == "" {
			d.Publisher = cleanText(common.FirstNonEmpty(jsonName(t["author"]), jsonName(t["publisher"]), jsonName(t["creator"]), jsonName(t["provider"])))
		}
		if d.Icon == "" {
			d.Icon = jsonName(t["image"])
			if img, ok := t["image"].(map[string]any); ok {
				d.Icon, _ = img["url"].(string)
			}
		}
	}
}

// jsonName reads a schema.org "Thing or Text" value.
func jsonName(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if n, ok := t["name"].(string); ok {
			return n
		}
	case []any:
		for _, c := range t {
			if n := jsonName(c); n != "" {
				return n
			}
		}
	}
	return ""
}

func cleanText(v string) string {
	return strings.Join(visibleLines(v), " ")
}

func cleanTitle(v string) string {
	return trimListingName(storeSuffixRE.ReplaceAllString(cleanText(v), ""))
}

func trimPublisher(v string) string {
	v = strings.TrimSpace(cleanText(v))
	if m := byLineRE.FindStringSubmatch(v); len(m) > 1 {
		v = strings.TrimSpace(m[1])
	}
	return common.TruncateRunes(v, 80)
}

// ---------------------------------------------------------------------------
// URL handling
// ---------------------------------------------------------------------------

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
	if (parsed.Host == "duckduckgo.com" || strings.HasSuffix(parsed.Host, ".duckduckgo.com") || (parsed.Host == "" && parsed.Path == "/l/")) && parsed.Path == "/l/" {
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

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

func visibleLines(fragment string) []string {
	fragment = scriptStyleRE.ReplaceAllString(fragment, " ")
	fragment = blockEndRE.ReplaceAllString(fragment, "\n")
	fragment = tagRE.ReplaceAllString(fragment, " ")
	fragment = html.UnescapeString(fragment)
	fragment = strings.ReplaceAll(fragment, "​", "")
	fragment = strings.ReplaceAll(fragment, " ", " ")
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

// isNoiseLine matches UI chrome that is neither a name nor a description:
// badges, buttons, ratings and user counts.
func isNoiseLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" || noiseLines[lower] {
		return true
	}
	if ratingLineRE.MatchString(lower) || ratedLineRE.MatchString(lower) {
		return true
	}
	if strings.Contains(lower, "add to chrome") || strings.Contains(lower, "add to chromium") {
		return true
	}
	if len(strings.Fields(lower)) < 4 && (strings.Contains(lower, " users") || strings.Contains(lower, "rating")) {
		return true
	}
	return false
}

func chooseName(lines []string) string {
	for _, line := range lines {
		if isNoiseLine(line) || len([]rune(line)) > 140 {
			continue
		}
		return line
	}
	return ""
}

func chooseDescription(lines []string, name string) string {
	parts := make([]string, 0, 2)
	for _, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), strings.TrimSpace(name)) || isNoiseLine(line) {
			continue
		}
		parts = append(parts, common.TruncateRunes(line, 260))
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func trimListingName(s string) string {
	s = storeSuffixRE.ReplaceAllString(strings.TrimSpace(s), "")
	s = strings.TrimSpace(strings.Trim(s, "|·•-–—"))
	for _, sep := range []string{"\n", " | ", " · ", " — ", " – "} {
		if i := strings.Index(s, sep); i > 0 && i < 140 {
			s = s[:i]
		}
	}
	return common.TruncateRunes(s, 100)
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
	for _, re := range snippetBlockRE {
		match := re.FindStringSubmatch(contextHTML)
		if len(match) > 1 {
			lines := visibleLines(match[1])
			if len(lines) > 0 {
				return common.TruncateRunes(strings.Join(lines, " "), 360)
			}
		}
	}
	return ""
}

func extractIcon(inner string) string {
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
