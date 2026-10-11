package firefox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cws-lite/common"
)

func fakeAMO(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/addons/search/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("app") != "firefox" || q.Get("q") != "dark" {
			http.Error(w, "bad query", 400)
			return
		}
		if q.Get("type") == "statictheme" {
			_, _ = w.Write([]byte(`{"results":[{"id":9,"slug":"night-sky","name":"Night Sky","summary":"A dark theme.","authors":[{"name":"Ada"}],"icon_url":"https://addons.mozilla.org/i.png","url":"https://addons.mozilla.org/en-US/firefox/addon/night-sky/"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[
		 {"id":1,"slug":"dark-reader","name":{"en-US":"Dark Reader","de":"Dunkel"},"summary":{"en-US":"<b>Dark</b> mode &amp; more"},
		  "authors":[{"name":"Dark Reader Ltd","username":"dr"}],"icon_url":"https://addons.mozilla.org/icon.png","url":"https://evil.example/x"},
		 {"id":2,"slug":"dark-reader","name":"dup"},
		 {"id":3,"slug":"bad/slug","name":"skipme"}]}`))
	})
	mux.HandleFunc("/api/v5/addons/addon/dark-reader/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"slug":"dark-reader","current_version":{"file":{"url":"` + "http://" + r.Host + `/files/dr.xpi"}}}`))
	})
	mux.HandleFunc("/api/v5/addons/addon/broken/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":2,"slug":"broken","current_version":{"file":{"url":"https://evil.example/x.xpi"}}}`))
	})
	mux.HandleFunc("/files/dr.xpi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("PK\x03\x04rest-of-zip"))
	})
	srv := httptest.NewServer(mux)
	oldBase, oldAllowed := apiBase, downloadURLAllowed
	apiBase = srv.URL
	downloadURLAllowed = func(u *url.URL) bool { return u.Host == strings.TrimPrefix(srv.URL, "http://") }
	t.Cleanup(func() { srv.Close(); apiBase, downloadURLAllowed = oldBase, oldAllowed })
	return srv
}

func TestSearch(t *testing.T) {
	fakeAMO(t)
	got := Search(context.Background(), "dark", common.KindExtensions)
	if got.Store != "firefox" || len(got.Results) != 1 {
		t.Fatalf("%#v", got)
	}
	l := got.Results[0]
	if l.ID != "dark-reader" || l.Name != "Dark Reader" || l.Publisher != "Dark Reader Ltd" || l.Description != "Dark mode & more" {
		t.Fatalf("bad listing: %#v", l)
	}
	if l.URL != "https://addons.mozilla.org/en-US/firefox/addon/dark-reader/" {
		t.Fatalf("untrusted listing URL kept: %s", l.URL)
	}
	themes := Search(context.Background(), "dark", common.KindThemes)
	if len(themes.Results) != 1 || themes.Results[0].Kind != "themes" || themes.Results[0].Icon == "" {
		t.Fatalf("%#v", themes)
	}
}

func TestSearchUpstreamFailure(t *testing.T) {
	srv := fakeAMO(t)
	srv.Close()
	got := Search(context.Background(), "dark", common.KindExtensions)
	if len(got.Results) != 0 || got.Note == "" {
		t.Fatalf("expected a note on failure: %#v", got)
	}
}

func TestDownload(t *testing.T) {
	fakeAMO(t)
	rec := httptest.NewRecorder()
	Download(rec, httptest.NewRequest("GET", "/api/download?store=firefox&id=dark-reader&name=Dark%20Reader", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "PK") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/x-xpinstall" || !strings.Contains(rec.Header().Get("Content-Disposition"), "Dark-Reader.xpi") {
		t.Fatalf("headers: %v", rec.Header())
	}
}

func TestDownloadRejects(t *testing.T) {
	fakeAMO(t)
	for _, target := range []string{"/api/download?id=../etc", "/api/download?id=broken", "/api/download?id=missing"} {
		rec := httptest.NewRecorder()
		Download(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code < 400 {
			t.Errorf("%s should fail, got %d", target, rec.Code)
		}
	}
}
