package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"cws-lite/common"
)

func TestSearchValidation(t *testing.T) {
	cases := map[string]int{
		"/api/search?q=x&store=nope":                400,
		"/api/search?q=x&type=widgets":              400,
		"/api/search?q=":                            400,
		"/api/search?q=" + strings.Repeat("a", 121): 400,
	}
	for target, want := range cases {
		rec := httptest.NewRecorder()
		handleSearch(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != want {
			t.Errorf("%.40s: got %d want %d", target, rec.Code, want)
		}
	}
}

func TestDownloadUnknownStore(t *testing.T) {
	rec := httptest.NewRecorder()
	handleDownload(rec, httptest.NewRequest("GET", "/api/download?store=nope&id=x", nil))
	if rec.Code != 400 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestCachePutGet(t *testing.T) {
	c := newSearchCache()
	c.put("k", common.SearchResponse{Query: "q"})
	if got, ok := c.get("k"); !ok || got.Query != "q" {
		t.Fatal("cache miss")
	}
}
