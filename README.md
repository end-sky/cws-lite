# CWS Lite

A small, self-hosted front end for browsing Chrome Web Store extensions and themes. It is written in Go and plain HTML/CSS/JavaScript, uses only the Go standard library, and embeds its frontend into the executable. No database, account, Node.js, or external frontend assets are required.

## Features

- Search modes for **Extensions** and **Themes**.
- Scrapes public Chrome Web Store search-page HTML first, then falls back to DuckDuckGo and Bing public HTML search results if the Store page cannot be parsed.
- Each result shows title, publisher and description; anything the search page lacks (always the publisher) is filled in from the listing's own page.
- Downloads the original `.crx` package from Google's official update endpoint; no third-party CRX mirrors are involved.
- Light/dark appearance toggle in the top-right corner.
- Small in-memory search cache to avoid fetching the same query repeatedly.
- Built-in installation/search troubleshooting for Chromium and Ungoogled Chromium.
- Localhost-only by default, with a Nix flake for packaging.

## Run

Go 1.23 or newer is recommended.

```sh
go run .
```

Then open <http://127.0.0.1:8080>.

Build a standalone binary:

```sh
go build -trimpath -ldflags='-s -w' -o cws-lite .
./cws-lite
```

Run tests:

```sh
go test ./...
```

### Configuration

- `ADDR` changes the listen address. Default: `127.0.0.1:8080`. Use `ADDR=0.0.0.0:8080` only if you intentionally want other devices on your network to access the UI.
- `CWS_PRODVERSION` overrides the Chrome version sent to Google's CRX update endpoint. By default, CWS Lite extracts the version from the requesting browser's user agent, falling back to `156.0.0.0`. Use four numeric version components, such as `156.0.0.0`.

## Code layout

- `main.go` – HTTP server, API handlers, search cache, CRX download.
- `scrapers.go` – all Web Store / search-index fetching and parsing, plus detail-page enrichment.
- `web/index.html` – embedded frontend.

## Nix

Build with flakes enabled:

```sh
nix build
./result/bin/cws-lite
```

Or launch without building a package first:

```sh
nix develop
 go run .
```

## Installing a download

CWS Lite serves CRX packages using the extension MIME type so compatible Ungoogled Chromium builds can show their native install prompt when the flag is enabled. Other browsers may save the file instead; CWS Lite never installs add-ons silently, and the browser controls the final prompt.

### Ungoogled Chromium

1. Open `chrome://flags/#extension-mime-request-handling`.
2. Set **Handling of extension MIME type requests** to **Always prompt for install**, if the flag is available.
3. Relaunch the browser, then open a CWS Lite `.crx` download. If no prompt appears, open `chrome://extensions`, enable **Developer mode**, and drag the downloaded file onto that page.

If the flag is missing, use the Developer mode drag-and-drop method. Some desktop environments or browser builds may behave differently.

### Chromium / Google Chrome

Try opening `chrome://extensions`, enabling **Developer mode**, and dragging the downloaded `.crx` onto the page. Some official Chrome builds, operating systems, or managed policies reject local CRX installs. In those cases, use the official listing's normal installation flow instead of changing managed-device policies.

Themes are distributed as CRX packages too, but the exact prompt varies by browser build.

## Troubleshooting

**Search shows no results.** The Chrome Web Store is a JavaScript-heavy site and its markup changes. CWS Lite tries the public Store page and then a public search-index fallback; either source can be temporarily blocked, rate-limited, or incomplete. Try another query, check the server's internet/proxy access, or open the [official Web Store](https://chromewebstore.google.com/).

**Google returns HTTP 204 / a failed download.** The item may have been removed, be restricted, or not have a compatible package available from the endpoint. Check the official listing and try again later. Avoid third-party CRX mirror sites when a Store listing exists.

**“Invalid package”, no install prompt, or the download opens as text.** Right-click **Download .crx**, select **Save link as**, and save the file to disk. Confirm that it is not zero bytes. Then use the flag or Developer mode steps above.

**The browser says an extension is blocked.** Browser policy and OS rules take precedence over CWS Lite. A local front end cannot override those policies.

## How the scraper works

The search handler requests the public Store search URL with a normal browser-like user agent and parses Store listing links from the returned HTML. If that response has no parseable listings, it queries DuckDuckGo and then Bing with a `site:chromewebstore.google.com/detail` filter. Those fallback paths are indexed-result fallbacks, not direct Store result feeds, and are labeled as such in the UI. The scraper does not attempt to solve CAPTCHAs, bypass logins, or evade anti-bot challenges.

Downloads are restricted to IDs in Chrome's 32-character `a`–`p` format and sent to `https://clients2.google.com/service/update2/crx`. The response is checked for the `Cr24` CRX signature before it is streamed to the browser.

## License

MIT. See `LICENSE`.
