package google

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"troller/common"
)

const defaultProdVersion = "156.0.0.0"

var (
	prodVersionRE      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	userAgentVersionRE = regexp.MustCompile(`(?i)(?:Chrome|Chromium)/([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)`)
)

// Download streams the original CRX package for ?id= from Google's update endpoint.
func Download(w http.ResponseWriter, r *http.Request) {
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
	filename := common.SafeFilename(name) + ".crx"
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
	req.Header.Set("User-Agent", common.UserAgent)
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
