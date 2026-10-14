// Package firefox searches and downloads from Firefox Add-ons (addons.mozilla.org)
// through Mozilla's public, documented JSON API (v5). No HTML scraping is needed.
package firefox

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"troller/common"
)

// Overridable in tests.
var apiBase = "https://addons.mozilla.org"

var (
	tagRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE = regexp.MustCompile(`\s+`)
	idRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// localized accepts both `"text"` and `{"en-US": "text", "de": "..."}`.
type localized string

func (l *localized) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = localized(s)
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		*l = ""
		return nil // tolerate odd shapes rather than failing the whole search
	}
	for _, key := range []string{"en-US", "en-GB", "en"} {
		if v := m[key]; v != "" {
			*l = localized(v)
			return nil
		}
	}
	for _, v := range m {
		if v != "" {
			*l = localized(v)
			break
		}
	}
	return nil
}

type addon struct {
	ID      int64     `json:"id"`
	Slug    string    `json:"slug"`
	Name    localized `json:"name"`
	Summary localized `json:"summary"`
	IconURL string    `json:"icon_url"`
	URL     string    `json:"url"`
	Authors []struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"authors"`
	CurrentVersion struct {
		File struct {
			URL string `json:"url"`
		} `json:"file"`
	} `json:"current_version"`
}

type searchPayload struct {
	Results []addon `json:"results"`
}

// Search queries addons.mozilla.org. kind is "extensions" or "themes".
func Search(ctx context.Context, query, kind string) common.SearchResponse {
	response := common.SearchResponse{Query: query, Store: "firefox", Kind: kind, Results: []common.Listing{}}

	addonType := "extension"
	if kind == common.KindThemes {
		addonType = "statictheme"
	}
	params := url.Values{}
	params.Set("q", query)
	params.Set("type", addonType)
	params.Set("app", "firefox")
	params.Set("lang", "en-US")
	params.Set("page_size", fmt.Sprint(common.MaxResults))

	var payload searchPayload
	if err := getJSON(ctx, apiBase+"/api/v5/addons/search/?"+params.Encode(), &payload); err != nil {
		response.Note = "Firefox Add-ons could not be reached (" + err.Error() + "). Try again in a moment or open addons.mozilla.org directly."
		return response
	}
	response.Results = listingsFrom(payload.Results, kind)
	response.Source = "Firefox Add-ons"
	if len(response.Results) == 0 {
		response.Note = "Firefox Add-ons returned no matches. Try a different or shorter search term."
	}
	return response
}

func listingsFrom(addons []addon, kind string) []common.Listing {
	out := make([]common.Listing, 0, len(addons))
	seen := map[string]bool{}
	for _, a := range addons {
		id := a.Slug
		if id == "" && a.ID != 0 {
			id = fmt.Sprint(a.ID)
		}
		if !idRE.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true

		name := cleanText(string(a.Name))
		if name == "" {
			name = id
		}
		publisher := ""
		if len(a.Authors) > 0 {
			publisher = common.FirstNonEmpty(a.Authors[0].Name, a.Authors[0].Username)
		}
		listingURL := a.URL
		if u, err := url.Parse(listingURL); err != nil || u.Scheme != "https" || u.Hostname() != "addons.mozilla.org" {
			listingURL = "https://addons.mozilla.org/en-US/firefox/addon/" + url.PathEscape(id) + "/"
		}
		icon := a.IconURL
		if u, err := url.Parse(icon); err != nil || u.Scheme != "https" || u.Hostname() == "" {
			icon = ""
		}
		out = append(out, common.Listing{
			ID:          id,
			Name:        common.TruncateRunes(name, 100),
			Publisher:   common.TruncateRunes(cleanText(publisher), 80),
			Description: common.TruncateRunes(cleanText(string(a.Summary)), 360),
			Icon:        icon,
			URL:         listingURL,
			Kind:        kind,
		})
	}
	return out
}

// cleanText strips markup and entities from API strings (summaries may contain HTML).
func cleanText(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}

func getJSON(ctx context.Context, rawURL string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, common.SearchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", common.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, common.SearchBodyLimit))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// downloadURLAllowed restricts where an .xpi may be fetched from. Overridable in tests.
var downloadURLAllowed = func(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "addons.mozilla.org" || host == "addons.cdn.mozilla.net")
}

// Download streams the current signed .xpi of the add-on named by ?id= (slug or numeric id).
// It is served as application/x-xpinstall so Firefox offers to install it.
func Download(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if !idRE.MatchString(id) {
		http.Error(w, "Invalid Firefox add-on ID.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var detail addon
	if err := getJSON(ctx, apiBase+"/api/v5/addons/addon/"+url.PathEscape(id)+"/?lang=en-US", &detail); err != nil {
		http.Error(w, "Could not look up that add-on on addons.mozilla.org ("+err.Error()+").", http.StatusBadGateway)
		return
	}
	fileURL, err := url.Parse(detail.CurrentVersion.File.URL)
	if err != nil || !downloadURLAllowed(fileURL) {
		http.Error(w, "This add-on has no downloadable package.", http.StatusBadGateway)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL.String(), nil)
	if err != nil {
		http.Error(w, "Could not prepare the download request.", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", common.UserAgent)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || !downloadURLAllowed(req.URL) {
			return fmt.Errorf("redirect to %s refused", req.URL.Host)
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Download failed while contacting Mozilla. Check your connection or try again later.", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Mozilla returned HTTP %d for this package.", resp.StatusCode), http.StatusBadGateway)
		return
	}
	head := make([]byte, 2)
	if _, err := io.ReadFull(resp.Body, head); err != nil || string(head) != "PK" {
		http.Error(w, "Mozilla did not return a valid .xpi package.", http.StatusBadGateway)
		return
	}

	name := common.SafeFilename(common.FirstNonEmpty(r.URL.Query().Get("name"), id))
	w.Header().Set("Content-Type", "application/x-xpinstall")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.xpi"`, name))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(head)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 200<<20))
}
