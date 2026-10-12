# Issue: clone silently saved a "403 Forbidden" page instead of the real site

## Symptom

Cloning `https://satvasoftech.com/process/client-onboarding-process/` completed
without any error, but `index.html` in the output project contained only:

```html
<html><head></head><body>403 - Forbidden | Access to this page is forbidden.
</body></html>
```

No CSS/JS/images were present either. Other sites cloned fine with the same
command, so the bug wasn't obviously in goclone's request logic itself.

## Investigation

Fetching the URL directly (curl, both with Go's default User-Agent and a real
browser User-Agent) returned the same result:

```
HTTP/1.1 202 Accepted
SG-Captcha: challenge
Set-Cookie: nevercache-b39818=Y;Max-Age=-1
```
```html
<meta http-equiv="refresh" content="0;/.well-known/sgcaptcha/?r=...">
```

Following that redirect manually landed on a "Robot Challenge Screen" — a
JS proof-of-work challenge served by SiteGround's anti-bot protection
(`SG-Captcha`). Key finding: the `Set-Cookie` header on every hop has
`Max-Age=-1`, meaning it expires immediately — it is never actually usable by
a client. The challenge requires executing JavaScript to compute a hash and
get redirected to the real page; a plain `net/http` client (or curl, or any
non-JS HTTP client) can never pass it, regardless of headers/cookies sent.

After enough failed/bot-like attempts, the WAF starts returning a hard `403`
instead of the challenge page — which is what got saved as `index.html`,
because goclone's `HTMLExtractor`/`Extractor` treated any HTTP response as
success and wrote the body to disk unconditionally.

## Root cause

`pkg/crawler/html.go` (`HTMLExtractor`) and `pkg/crawler/extractor.go`
(`Extractor`/`writeFileToPath`) never inspected the response — any status
code and any body (challenge pages, 403 pages, rate-limit pages) was written
to disk as if it were the real page/asset. There was no way to tell a
successful clone from a blocked one without opening the output and reading it.

Separately (noted, not yet fixed): both functions call `http.Get(link)`
directly, bypassing the `userAgent`/`cookieJar`/`proxyString` options that
`Collector` sets up for colly. This isn't why this specific site fails (its
protection is JS/proof-of-work based, not UA-based — confirmed via curl), but
it's an inconsistency worth fixing later.

## Options considered

1. **Detect the block and fail loudly** (cheap, no new dependencies) — recognize
   known anti-bot/WAF signatures and abort with a clear error instead of
   silently writing garbage.
2. **Headless browser fallback** (real fix for JS-gated sites) — drive actual
   headless Chrome (e.g. via `chromedp`) so JS challenges resolve the same way
   they would for a real visitor, then extract the rendered page. Heavier:
   new Chrome/Chromium dependency, slower clones, and would need concurrency
   throttling since WAFs also flag rapid parallel asset requests.

Decision: implement (1) first since it's low-risk and immediately stops silent
bad output on *any* WAF-protected site, not just this one. If false positives
or if this class of site needs to be cloneable, move to (2).

## What was implemented (this change)

Added `pkg/crawler/challenge.go` with `detectBotChallenge(resp, body)`, which
flags a response as a bot challenge when it sees:

- `SG-Captcha` response header (SiteGround)
- `cf-mitigated` response header (Cloudflare)
- Known challenge-page markers in the body: `robot challenge screen`,
  `just a moment`, `attention required`, `checking your browser`,
  `ddos protection by`, `access denied`
- Status `403 Forbidden` or `429 Too Many Requests`

Wired into:

- `HTMLExtractor` (`pkg/crawler/html.go`) — if the **main page** is blocked,
  the clone aborts immediately with a descriptive error (e.g.
  `blocked by anti-bot challenge (SG-Captcha: challenge) - this site requires
  a JS-capable browser to pass`). Nothing is written to `index.html`.
- `writeFileToPath` (`pkg/crawler/extractor.go`) — if an individual **asset**
  (CSS/JS/image) response is blocked, that one file is skipped (logged as a
  warning, same as any other extraction failure) without aborting the whole
  crawl, and the response body is validated *before* the file is created on
  disk (previously an empty file could be left behind on failure).

## Verification

- `go build ./...` — compiles cleanly.
- `go test ./...` — all existing tests pass, no regressions.
- Ran against the real blocked URL:
  ```
  Downloading main HTML from: https://satvasoftech.com/process/client-onboarding-process/
  Extracting HTML from -->  https://satvasoftech.com/process/client-onboarding-process/
  "https://satvasoftech.com/process/client-onboarding-process/": failed to download main HTML:
  https://satvasoftech.com/process/client-onboarding-process/: blocked by anti-bot challenge
  (SG-Captcha: challenge) - this site requires a JS-capable browser to pass
  ```
  Exit code 1, no `index.html` written, no garbage saved.
- Ran against a normal, unprotected site (`https://example.com`) — cloned
  successfully as before (no false positive from the new check).

## Status after first pass

Option (1) alone (detect + fail loudly) worked as designed but didn't make
the site cloneable — it just stopped bad output. Decision was to move to
option (2).

## Option (2): headless-browser fallback (implemented)

Added a headless-Chrome fallback that only activates for sites that already
tripped `detectBotChallenge` — every other site keeps using the fast, plain
`net/http` path exactly as before, so this adds no overhead or new failure
modes for normal clones.

**New file `pkg/crawler/headless.go`** — `renderWithHeadlessBrowser(ctx, link,
userAgent)` launches a real (headless) Chrome via `chromedp`, navigates to the
URL, waits `challengeSettleWait` (6s) for any client-side JS challenge/redirect
to finish, then captures the final rendered `<html>` as a string. Bounded by a
30s total timeout (`headlessRenderTimeout`).

**`HTMLExtractor` (`pkg/crawler/html.go`)** — signature changed to
`(ctx context.Context, link, projectPath, userAgent string) (usedHeadless bool, finalHTML string, err error)`.
Flow:
1. Try the plain HTTP GET, exactly as before.
2. If `detectBotChallenge` flags it, fall back to `renderWithHeadlessBrowser`.
3. Re-run `detectBotChallenge` on the rendered result — if the browser still
   landed on a challenge/blocked page, return an error (headless fallback
   doesn't "solve" every possible protection, e.g. actual human captchas).
4. Otherwise write the rendered HTML to `index.html` and report
   `usedHeadless = true` + the HTML back to the caller.

**`Collector` (`pkg/crawler/collector.go`)** — when `usedHeadless` is true, the
colly collector used for link discovery is pointed at a new
`cachedResponseTransport` that returns the already-rendered HTML for any
request instead of hitting the network again. This matters because colly's
own `c.Visit(url)` would otherwise re-request the same URL over plain HTTP and
walk straight back into the same challenge, finding zero links. Sites that
never needed the headless path never install this transport and behave
exactly as before.

Dependency added: `github.com/chromedp/chromedp` (drives Chrome via the
DevTools Protocol; requires a Chrome/Chromium binary installed on the machine
running goclone — chromedp locates it automatically via common install
paths). `go.mod`'s Go directive was bumped 1.25 → 1.26 by `go get` to satisfy
chromedp's own `go.mod` requirement.

### Verification

- `go build ./...` and `go test ./...` — pass, no regressions.
- Unprotected sites (`https://example.com`, the local test-server suite in
  `crawler_test.go`) — unchanged behavior, headless path never triggered.
- `https://satvasoftech.com/process/client-onboarding-process/`:
  - **Main page**: now succeeds. Plain GET is detected as blocked, headless
    fallback runs, and the real ~104KB rendered page is saved to
    `index.html` (previously: a 27-byte `403 Forbidden` stub).
  - **Cross-origin assets** (Google Tag Manager, Microsoft Clarity, Apollo,
    CloudFront-hosted scripts, etc.) — download fine, since they aren't
    behind this site's WAF.
  - **Same-origin assets** (images and self-hosted JS under
    `satvasoftech.com/wp-content/...`) — **still individually blocked.**
    Each asset is fetched by `Extractor` as its own plain `http.Get`, which
    hits the same SG-Captcha wall independently of the main page. These are
    skipped with a warning (same graceful-degradation behavior as any other
    failed asset), not written as garbage.

### Why same-origin assets weren't fixed by this pass

I tested whether the challenge-passing session could be reused for those
plain HTTP asset requests: extracted the real cookies chromedp held after the
challenge resolved (a legitimate, long-lived `_I_=...` cookie appears — unlike
the earlier `nevercache-b39818` cookie which is a decoy sent with
`Max-Age=-1`), then replayed that cookie via curl against a blocked image
URL. Result: still `202` / `SG-Captcha: challenge`. So this WAF ties the pass
through to something beyond a replayable cookie (e.g. TLS/JA3 fingerprint or
other browser-only signal), not just session state — cookie reuse from
outside the browser doesn't work. This ruled out a cheap cookie-forwarding
fix and confirmed the only way to get these assets is to read them from
inside the browser session itself.

## False alarm: apparent regression on unrelated sites

After this first pass, a manual before/after comparison of
`satvasoftech.com` showed the cloned page rendering completely unstyled
(default blue links, expanded nav, no layout) next to the real site, and it
looked like other previously-fully-cloned sites had also started coming back
incomplete, raising the question of whether to roll the whole change back.

Investigated by re-cloning 5 previously-good sites (`zoolatech.com`,
`enstacked.com`, `soft-industry.com`, `saigontechnology.com`,
`www.gmtasoftware.com`) and diffing file counts against their existing
baseline clones already in the repo. Result: file counts matched (or exceeded,
likely due to site content changes since the baseline) in all cases, and the
only block that fired anywhere was a **genuine** 403 on a hotlink-protected
third-party badge image — not a false positive from the challenge-detection
markers. So there was no general regression; the screenshot was
`satvasoftech.com` itself, and the missing CSS was exactly the same-origin
gap already documented above, not new breakage. Decided to fix it properly
instead of rolling back (rollback would have brought that site back to a bare
403 stub for no benefit to the unaffected sites).

## Closing the same-origin gap: in-browser network capture (implemented)

Extended `renderWithHeadlessBrowser` (`pkg/crawler/headless.go`) to listen to
the browser's own network traffic while it renders the page (via
`chromedp.ListenTarget` + the CDP `Network` domain) and save every
CSS/JS/image/font response it receives straight to `projectPath`, using the
same directory/naming scheme as `Extractor` (`extensionDir`,
`uniqueFilename`, reusing `processCSS` for stylesheets). Those bytes come
from inside the already-challenge-passed browser session, so they're the
real content — no second, externally-blocked HTTP request is needed for
same-origin assets at all.

Mechanics:
- `network.Enable()` runs before `chromedp.Navigate`; `EventResponseReceived`
  records `requestID -> URL` for responses whose URL extension is one goclone
  cares about; `EventLoadingFinished` triggers a goroutine that fetches the
  body via `network.GetResponseBody` and writes it to disk.
- **Bug hit during implementation:** calling `network.GetResponseBody(id).Do(ctx)`
  directly from the `ListenTarget` callback failed with `invalid context`.
  Root cause: `chromedp.Run` normally injects a per-call CDP executor into
  the context; a context obtained any other way (including the one handed to
  `ListenTarget`) doesn't carry it. Fix (this is chromedp's documented pattern
  for exactly this situation): rebuild an executor-bound context before
  calling `Do` —
  `cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)`.
  Verified with a standalone script hitting the real blocked URL: before the
  fix, every `GetResponseBody` call errored; after it, real bytes came back
  for the CSS/font/image/JS responses observed.
- A `sync.WaitGroup` tracks in-flight body fetches; after the page settles,
  `renderWithHeadlessBrowser` waits up to `assetFlushWait` (3s) for them to
  finish before the browser context is torn down.
- Added `recordURLFilename` (`pkg/crawler/extractor.go`) with a mutex around
  the shared `urlToFilename` map, since it's now written concurrently from
  both colly's async asset callbacks and the new capture goroutines (this map
  write was already unguarded/racy before this change with colly's own
  concurrent callbacks; fixed while touching this path rather than adding a
  second unguarded writer).
- Existing ordering already makes this safe to combine with the plain-HTTP
  path: `Collector` runs `HTMLExtractor` (which does the capture) to
  completion *before* colly visits the page to discover links and re-fetch
  assets via `Extractor`. If colly's plain-HTTP re-fetch of a same-origin
  asset then gets blocked, `writeFileToPath` reads and validates the response
  *before* opening the destination file (see the first pass above), so the
  good, already-captured file on disk is left untouched — the failed retry
  just logs its usual warning.

### Verification

- `go build ./...` and `go test ./...` — pass, no regressions.
- `https://satvasoftech.com/process/client-onboarding-process/` — re-cloned:
  real main-page HTML, plus **CSS, fonts (`.woff`/`.woff2`), and same-origin
  images/JS all now saved with real content** (e.g. a 235KB real stylesheet,
  actual WOFF font binaries, real product images) instead of being skipped.
  Colly's later plain-HTTP re-fetch attempts for those same URLs still get
  blocked (expected, logged as warnings) but no longer clobber the good files
  already on disk.
- Re-cloned `https://example.com` — finishes in under a second, confirming
  the headless path still never launches for sites that don't trip the
  challenge detector.
- `go test ./...` suite unaffected.

### Known remaining limitation

Third-party sub-resources loaded *by* the target page from other
challenge-protected domains (not the target site's own domain) would only be
captured if they're loaded during the same render — not an issue seen in
practice so far, but noted as an edge case. Also, this fallback captures
whatever the browser actually requests during the settle window
(`challengeSettleWait`, 6s) — very slow-loading pages or resources fetched
lazily well after that window could still be missed; if that turns out to
matter, the fix is to wait on a network-idle signal instead of (or in
addition to) a fixed sleep.

---

# Separate issue: font-heavy pages timed out and lost their CSS

This is **not** an anti-bot problem — the site involved
(`sumatosoft.com/services/...`) has no WAF challenge — but it surfaced during
the same work, so it's documented here.

## Symptom

Cloning `https://sumatosoft.com/services/rag-development` (and
`/services/ai-agent-development-company`) produced a completely **unstyled**
page when served locally: default blue links, no layout, no fonts — even
though `index.html` and some assets downloaded.

## Root cause

Two compounding problems, both about how assets are fetched:

1. **All downloads were serial, and the site is slow (~2s/request).**
   - The `img[src]`/`script[src]`/`link[rel=stylesheet]` colly callbacks each
     called `Extractor` *synchronously*, and colly fires those callbacks one
     at a time in a single goroutine — so every asset downloaded one-by-one.
   - Worse, `processCSS` downloaded each `url(...)` asset inside a CSS file
     synchronously too. `global.css` alone references **65 font `url()`s**, so
     that one stylesheet blocked on ~65 serial 2-second downloads (~130s)
     before the *next* stylesheet was even looked at.
   - The page has ~200 assets total. At ~2s each, serial, the crawl ran for
     many minutes.

2. **No HTTP timeout.** `Extractor`/`HTMLExtractor` used `http.Get`
   (`http.DefaultClient`, which has *no* timeout), so a single slow or hung
   response could stall the crawl indefinitely.

Because the crawl never finished in a reasonable time, `c.Wait()` never
returned, `Collector` never returned, and **`html.LinkRestructure` (which
rewrites remote asset URLs to the local `css/`, `js/`, `imgs/`, `fonts/`
paths) never ran**. So `index.html` kept pointing at `https://sumatosoft.com/...`
URLs, and served offline it rendered unstyled. The missing CSS was a symptom
of the crawl not completing, not of a link-rewriting bug.

## Fixes (implemented)

All in `pkg/crawler/extractor.go` and `pkg/crawler/collector.go`:

- **Per-request timeout + shared client.** Introduced a package-level
  `httpClient` (`http.Client{Timeout: 30s}`, with the same
  `InsecureSkipVerify` transport the old code set globally). `Extractor` and
  `HTMLExtractor` both use it now. Also removed `HTMLExtractor`'s global
  mutation of `http.DefaultTransport`'s TLS config (a shared-global side
  effect) in favor of the client's own transport.

- **Concurrent asset downloads, bounded.** `processCSS` now downloads each
  referenced asset in its own goroutine, and the top-level colly callbacks
  hand each asset to an `extractAsync` helper (goroutine + a `sync.WaitGroup`
  the `Collector` waits on after `c.Wait()`). A package-level semaphore
  `downloadSem` (capacity 12) caps *actual* concurrency so we don't open
  hundreds of sockets or trip rate limiting. The semaphore slot is released
  **before** `Extractor` recurses into `processCSS`, so a stylesheet isn't
  holding a slot while waiting on its own (also-semaphore-bounded) nested font
  downloads — otherwise a set of CSS files could deadlock holding all slots.

- **Deduplication.** `markSeen(projectPath, link)` skips re-downloading an
  asset already fetched in this crawl (the same font family is referenced from
  many stylesheets). Keyed by `projectPath` so a multi-site run still writes
  shared assets into each project. Reset per crawl via `resetDownloadState()`
  (called at the top of `Collector`) so re-cloning into a cleaned directory
  downloads again — without the reset, the process-global map made the second
  of two sequential clones (e.g. the test suite) skip everything and write
  empty files.

- **Thread-safety.** The CSS-string rewriting in `processCSS` stays on the
  main goroutine (downloads are what's parallelized), and the rewrite is
  deterministic and independent of download success, so it's correct
  regardless of download ordering. The shared `urlToFilename` map already got
  a mutex in the earlier pass; `seenURLs` has its own.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` — all pass.
- `https://sumatosoft.com/services/rag-development`:
  - Before: crawl ran past a 4-minute timeout without finishing; link
    rewriting never ran; served page fully unstyled.
  - After: completes in **~1 minute**, exit 0. All 9 unique stylesheets, 68
    fonts, and 83 images download; `global.css`'s font refs are rewritten to
    `url('../fonts/...')`; `index.html`'s stylesheet links point at local
    `css/...`. **Rendered the local clone in headless Chrome — it now displays
    fully styled** (correct dark gradient hero, logo, nav, fonts, buttons),
    matching the live site.
- `https://satvasoftech.com/...` (the WAF/headless case above) — still works
  end to end (~18s), headless capture + localized CSS/fonts/images intact.
- `https://zoolatech.com`, `https://example.com` — still clone correctly and
  fast (~12s and ~1s), confirming no regression on normal sites.

### Known minor limitation

WordPress's "async CSS" pattern emits a `<noscript><link rel=stylesheet ...></noscript>`
fallback duplicate of each stylesheet. goquery parses `<noscript>` contents as
raw text, so `arrange` can't rewrite those inner tags — they stay pointing at
the remote URL. They're harmless in practice: they only activate with
JavaScript disabled, and they reference the very same file that the primary
(non-noscript) tag already loads locally. Not worth a regex hack over the
serialized HTML unless offline-with-JS-disabled fidelity becomes a
requirement.
