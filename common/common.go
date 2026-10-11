// Package common holds the types and small helpers shared by the store
// back ends (google/, firefox/) and the HTTP server.
package common

import (
	"regexp"
	"strings"
	"time"
)

const (
	// Timeouts and limits shared by every back end.
	SearchTimeout   = 9 * time.Second
	SearchBodyLimit = 6 << 20
	MaxResults      = 24

	// UserAgent is sent to upstream stores.
	UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
)

// Kind values accepted by the API.
const (
	KindExtensions = "extensions"
	KindThemes     = "themes"
)

// Listing is one search result, identical for every store.
type Listing struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Publisher   string `json:"publisher,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	URL         string `json:"url"`
	Kind        string `json:"kind"`
}

// SearchResponse is the JSON returned by /api/search.
type SearchResponse struct {
	Query   string    `json:"query"`
	Store   string    `json:"store"`
	Kind    string    `json:"kind"`
	Source  string    `json:"source,omitempty"`
	Note    string    `json:"note,omitempty"`
	Results []Listing `json:"results"`
}

var unsafeFilename = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// SafeFilename turns an arbitrary title into a safe download file stem.
func SafeFilename(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "-")
	value = unsafeFilename.ReplaceAllString(value, "-")
	value = strings.Trim(value, ".-_ ")
	if value == "" {
		return "store-item"
	}
	return TruncateRunes(value, 72)
}

// TruncateRunes trims and shortens value to at most max runes, adding an ellipsis.
func TruncateRunes(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}

// FirstNonEmpty returns the first value that is not blank after trimming.
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
