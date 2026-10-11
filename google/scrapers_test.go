package google

import (
	"cws-lite/common"
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

func TestStoreCardWithPublisherAndNoise(t *testing.T) {
	doc := `<a class="q6LNgd" href="./detail/dark-reader/` + testExtensionID + `?hl=en-US&utm_source=ext_sidebar">
	  <img src="https://lh3.googleusercontent.com/abc" alt="">
	  <div><h2 class="CiI2if">Dark Reader</h2>
	  <span>Featured</span><span>4.7</span><span>(12K ratings)</span>
	  <span>Offered by Dark Reader Ltd</span>
	  <p>Dark mode for every website. Take care of your eyes.</p></div></a>`
	got := extractStoreResults([]byte(doc), "extensions", "https://chromewebstore.google.com/search/x")
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	l := got[0]
	if l.Name != "Dark Reader" || l.Publisher != "Dark Reader Ltd" || l.Description != "Dark mode for every website. Take care of your eyes." {
		t.Fatalf("bad card parse: %#v", l)
	}
	if strings.Contains(l.URL, "utm_source") {
		t.Fatalf("tracking params kept: %s", l.URL)
	}
}

func TestEmbeddedStoreData(t *testing.T) {
	doc := `<script>AF_initDataCallback({key: 'ds:1', hash: '2', data:[[["` + testExtensionID + `",null,"https://lh3.googleusercontent.com/icon","Cool Theme",4.6,120,null,"A really cool theme for your browser, with nice colors."]]], sideChannel: {}});</script>`
	got := extractStoreResults([]byte(doc), "themes", "https://chromewebstore.google.com/search/x")
	if len(got) != 1 || got[0].Name != "Cool Theme" || !strings.Contains(got[0].Description, "really cool theme") || got[0].Icon == "" {
		t.Fatalf("embedded data not parsed: %#v", got)
	}
}

func TestDuckDuckGoTitleAndSnippetMerge(t *testing.T) {
	enc := "https%3A%2F%2Fchromewebstore.google.com%2Fdetail%2Fublock%2F" + testExtensionID
	doc := `<div class="result"><h2><a class="result__a" href="//duckduckgo.com/l/?uddg=` + enc + `&rut=x">uBlock Origin - Chrome Web Store</a></h2>
	<a class="result__snippet" href="//duckduckgo.com/l/?uddg=` + enc + `&rut=x">Finally, an <b>efficient</b> blocker for Chromium.</a></div>`
	got := extractStoreResults([]byte(doc), "extensions", "https://html.duckduckgo.com/html/?q=x")
	if len(got) != 1 || got[0].Name != "uBlock Origin" || got[0].Description != "Finally, an efficient blocker for Chromium." {
		t.Fatalf("ddg merge failed: %#v", got)
	}
}

func TestParseDetailPageJSONLD(t *testing.T) {
	doc := `<html><head><title>Foo - Chrome Web Store</title>
	<script type="application/ld+json">{"@type":"SoftwareApplication","name":"Foo","description":"Does foo.","author":{"@type":"Organization","name":"Foo Inc"}}</script></head></html>`
	d := parseDetailPage([]byte(doc))
	if d.Name != "Foo" || d.Description != "Does foo." || d.Publisher != "Foo Inc" {
		t.Fatalf("%#v", d)
	}
}

func TestParseDetailPageMetaAndOfferedBy(t *testing.T) {
	doc := `<html><head><title>Bar - Chrome Web Store</title>
	<meta property="og:title" content="Bar - Chrome Web Store"><meta content="Bar does bar &amp; more." property="og:description"></head>
	<body><div>Offered by</div><div><a>Bar Team</a></div></body></html>`
	d := parseDetailPage([]byte(doc))
	if d.Name != "Bar" || d.Description != "Bar does bar & more." || d.Publisher != "Bar Team" {
		t.Fatalf("%#v", d)
	}
	doc2 := `<body><span>Offered by: Inline Co</span></body>`
	if p := parseDetailPage([]byte(doc2)).Publisher; p != "Inline Co" {
		t.Fatalf("inline publisher: %q", p)
	}
}

func TestApplyDetailKeepsExistingWhenEmpty(t *testing.T) {
	l := common.Listing{Name: "A", Description: "desc"}
	applyDetail(&l, detailInfo{Publisher: "P"})
	if l.Name != "A" || l.Description != "desc" || l.Publisher != "P" {
		t.Fatalf("%#v", l)
	}
}
