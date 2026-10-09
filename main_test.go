package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

const testExtensionID = "abcdefghijklmnopabcdefghijklmnop"

func TestExtractStoreResults(t *testing.T) {
	htmlDoc := `<html><body>
		<a href="https://chromewebstore.google.com/detail/super-tool/abcdefghijklmnopabcdefghijklmnop">
		  <img src="https://example.com/icon.png" alt="Super Tool icon">
		  <div><span>Super Tool</span><p>A small tool that makes browser work easier.</p></div>
		</a>
		<a href="https://example.com/not-a-store-item">Ignore me</a>
	</body></html>`
	results := extractStoreResults([]byte(htmlDoc), "extensions", "https://chromewebstore.google.com/search/tool")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %#v", len(results), results)
	}
	got := results[0]
	if got.ID != testExtensionID {
		t.Fatalf("wrong ID: %q", got.ID)
	}
	if got.Name != "Super Tool" {
		t.Fatalf("wrong name: %q", got.Name)
	}
	if !strings.Contains(got.Description, "small tool") {
		t.Fatalf("description was not parsed: %q", got.Description)
	}
	if got.Kind != "extensions" {
		t.Fatalf("wrong kind: %q", got.Kind)
	}
}

func TestExtractIDFromURL(t *testing.T) {
	cases := []struct{ input, want string }{
		{"https://chromewebstore.google.com/detail/a-name/" + testExtensionID, testExtensionID},
		{"https://chrome.google.com/webstore/detail/a-name/" + testExtensionID + "?hl=en", testExtensionID},
		{"https://example.com/detail/a-name/" + testExtensionID, ""},
		{"https://chromewebstore.google.com/detail/invalid-id", ""},
	}
	for _, tc := range cases {
		if got := extractIDFromURL(tc.input); got != tc.want {
			t.Errorf("extractIDFromURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeDuckDuckGoHref(t *testing.T) {
	href := "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fchromewebstore.google.com%2Fdetail%2Fname%2F" + testExtensionID + "%3Fhl%3Den&rut=ignored"
	got := normalizeStoreHref(href, "https://html.duckduckgo.com/html/?q=test")
	if got == "" || extractIDFromURL(got) != testExtensionID {
		t.Fatalf("could not unwrap DDG result URL: %q", got)
	}
}

func TestNormalizeBingHref(t *testing.T) {
	// Bing's common a1 redirect token is URL-safe base64 of the destination URL.
	target := "https://chromewebstore.google.com/detail/name/" + testExtensionID
	encoded := base64.RawURLEncoding.EncodeToString([]byte(target))
	href := "https://www.bing.com/ck/a?u=a1" + encoded + "&ntb=1"
	got := normalizeStoreHref(href, "https://www.bing.com/search?q=test")
	if got == "" || extractIDFromURL(got) != testExtensionID {
		t.Fatalf("could not unwrap Bing result URL: %q", got)
	}
}

func TestSafeFilename(t *testing.T) {
	got := safeFilename(`../../ Very nice: extension!`)
	if got == "" || strings.ContainsAny(got, `/\\:`) || strings.Contains(got, "..") {
		t.Fatalf("unsafe filename: %q", got)
	}
}
