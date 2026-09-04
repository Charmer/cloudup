# Cloud Uploader

*[Читать по-русски](README.ru.md)*

A local REST API server (Go) plus a browser-based frontend (Vue 3, in
[`frontend/`](frontend)) for uploading files to multiple cloud storage
providers **in parallel**.

The primary way to use it is as a **desktop app** on your own machine —
download it, run it, a system tray icon appears and a browser tab opens
itself. Running it headless as a server (on a server/in a container/behind
systemd, embedded into another backend that just calls its REST API) is an
**additional, optional** capability the same binary also supports, no
separate build — which mode it starts in is auto-detected from the
environment (a graphical session vs. none), or forced with
`-gui`/`-gui=false`.

[![Go Reference](https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Why

Uploading the same file (or a batch of files) to several cloud storages
usually means several separate tools, each with its own auth flow, its own
retry logic, and no shared record of what actually got uploaded where. Cloud
Uploader is a single application that:

- uploads to several storages **in parallel** (one queue per connected
  storage, running concurrently; files within one storage upload
  sequentially, one at a time);
- keeps a **local, verifiable history** of every upload — including a
  checksum — so you can later confirm a file is still present and unmodified
  on the remote storage, even if someone deleted or replaced it there
  manually;
- stores credentials in the **OS keychain** (Windows Credential Manager /
  macOS Keychain / Linux Secret Service), never in a plaintext config file;
- is **fully portable**: every other bit of state (connections, history,
  settings, watch rules, logs) lives in one `data/` folder next to the
  binary itself, never in an OS-standard per-user location - move or copy
  that whole folder anywhere and it keeps working, and two separate
  installs on the same machine never accidentally share state;
- can **watch a local file or folder** and enqueue changes automatically
  (`internal/watch`, `GET`/`POST /api/v1/watches`) — the same queue, progress
  and history as a manual upload, no separate code path;
- checks for a newer release **only when you ask it to** (Settings → "Check
  for updates") — cloudup never contacts the network on its own initiative
  otherwise, and this is no exception;
- is built to make adding a **new storage backend** a matter of writing one
  new package, not touching the core;
- switches to a storage's **chunked-upload** path for large files
  automatically (S3 multipart, Dropbox upload sessions), reporting one
  continuous progress bar and one checksum for the whole file either way;
- for S3 and Backblaze B2, can additionally spread one large file's chunks
  across several concurrent connections (**multi-thread-streams**, like
  rclone's flag of the same name) — configurable in Settings, off above a
  size threshold rather than always-on;
- documents its entire API as an **OpenAPI spec** ([`openapi.yaml`](openapi.yaml))
  so anyone can point their own frontend at it instead of using the bundled
  one.

## Supported storage backends

| Provider | Notes |
|---|---|
| **Google Drive** | OAuth2 (Drive API v3) |
| **Amazon S3** and S3-compatible | MinIO, Backblaze B2, Wasabi, Yandex Object Storage, via a configurable endpoint |
| **WebDAV** | Nextcloud, ownCloud, box.com, and any standard WebDAV server |
| **Dropbox** | OAuth2 (Dropbox API v2) |
| **Backblaze B2** | native b2api v2 — server-side SHA1 makes checksum verification a cheap metadata read |
| **Yandex.Disk** | native REST API v1, OAuth2 — server-side MD5 makes checksum verification a cheap metadata read, unlike its generic WebDAV interface |
| **OneDrive / SharePoint** | OAuth2 (Microsoft Graph API v1.0) — optional `driveId` targets a specific SharePoint document library instead of the personal drive |
| **FTP** | RFC 959, with optional explicit TLS (FTPS) for servers that require an encrypted channel |
| **SFTP** | SSH File Transfer Protocol — password authentication only for now, host key checking not yet implemented (see "Known gaps" below) |

Adding another backend means implementing one interface
(`provider.Provider`, see
[CONTRIBUTING.md](CONTRIBUTING.md#composition-of-small-interfaces-not-one-large-one))
in a new package — the queue, history, and REST API code never change.

## Project status

**Everything below is implemented and tested.**

- ✅ **Core** — provider interfaces, nine storage backends, secrets, config,
  SQLite history, upload queue with retry/pause/cancel, local folder
  watching.
- ✅ **REST API** (`internal/httpapi`, `cmd/server`) — connections, uploads,
  queue (polled progress), history (paginated), settings, watches, and a
  generic OAuth flow (Google Drive, Dropbox, Yandex.Disk, OneDrive) —
  documented in [`openapi.yaml`](openapi.yaml).
- ✅ **Frontend** (`frontend/`, Vue 3 + Vite) — a reference UI covering
  every endpoint. Served by `cmd/server` itself by default (single process,
  single port, browser opens automatically), or runnable standalone against
  any cloudup server.
- ✅ **Desktop and service modes from one binary** — tray icon + auto-opened
  browser when a graphical session is present, plain headless service
  otherwise (`-gui`, auto-detected).

**Known gaps**:
- `internal/providers/googledrive`'s upload/download/list/delete methods
  aren't covered by integration tests (no good in-memory fake for the Drive
  v3 wire protocol was available, unlike WebDAV/S3) — verify against a real
  Google account before relying on it in production.
- `internal/providers/onedrive`'s live OAuth flow and Graph API behavior
  (folder auto-vivification, chunk-size boundaries, download redirect
  format) are implemented from the official docs but not yet confirmed
  against a real Azure AD app registration.
- `internal/providers/sftp` does not verify the server's host key (no
  known_hosts-style store or UI to pin/confirm a fingerprint exists yet), and
  authenticates by password only — public-key auth isn't wired in.
- A handful of manual-only checks remain undone: real clicks on the system
  tray icon, a `-H=windowsgui` build, and real headless auto-detection over
  SSH.

## Getting started

### Download a prebuilt release

Each [GitHub Release](https://github.com/Charmer/cloudup/releases) ships a
zip per platform (Windows amd64, macOS Apple Silicon, Linux amd64/arm64 -
no Intel Mac build), built by
[`.github/workflows/release.yml`](.github/workflows/release.yml) from a
pushed `vX.Y.Z` tag. Each zip already contains the built frontend and
`openapi.yaml` alongside the binary, laid out so the defaults just work —
extract it and run the binary from inside the extracted folder, no flags
needed. Windows zips carry two binaries: `cloudup-server.exe` (built with
`-H=windowsgui` — no console window, the one most people want) and
`cloudup-server-console.exe` (a plain build, if you'd rather watch live log
output in a terminal than tail `server.log`).

### Build from source

Requires Go 1.22+ and Node 18+ (for the frontend).

```bash
git clone https://github.com/<your-username>/cloudup.git
cd cloudup
go build ./...
go test ./...
```

### Running the app

Build the frontend once, then run the server — it serves the frontend
itself, opens your browser, and (by default) runs as a **system tray app**
instead of a plain foreground process: a tray icon appears, left-clicking
it reopens the browser if you closed the tab, right-clicking it shows an
"Exit" item that shuts the whole app down. Closing the browser tab does
*not* stop the server - the tray icon is what's actually running it.

```bash
cd frontend && npm install && npm run build && cd ..
go run ./cmd/server
```

Note for iterating locally: `go run` compiles to a throwaway binary in a
temp directory that's deleted once the process exits, and cloudup's state
lives in a `data/` folder next to whatever binary is actually running (see
[CONTRIBUTING.md](CONTRIBUTING.md#all-local-state-lives-in-one-portable-folder)) - so state made with `go run` doesn't
survive to the next `go run`. Build a real binary
(`go build -o cloudup-server ./cmd/server && ./cloudup-server`) for
anything that needs to persist across restarts.

This starts on `http://127.0.0.1:3000`, opens it in your default browser
already signed in (the API token is injected into the page - no
copy-pasting), and logs to `data/server.log` next to the binary instead of
the console (there normally isn't one to see - see the Windows build note
below).

To make the "Check for updates" button on the Settings page report a real
version instead of `dev`, build with the version tag embedded:

```bash
go build -ldflags "-X main.version=$(git describe --tags)" -o cloudup-server ./cmd/server
```

`-update-repo owner/repo` (default `Charmer/cloudup`) points that check at
a different GitHub repository - useful if you run a fork. This is the only
outbound call cloudup ever makes on its own; it never happens without a
click.

**On Windows**, `go run`/a plain `go build` still shows a console window
behind the tray icon, because that's controlled by the executable's PE
subsystem, not by anything cloudup does at runtime. To get a real
no-console experience, build with:

```bash
go build -ldflags="-H=windowsgui" -o cloudup-server.exe ./cmd/server
```

**If it doesn't start at all** - most commonly because it was extracted
into a folder you don't have write access to (`Program Files`, for
example: cloudup needs to create a `data/` folder next to its own binary,
see
[CONTRIBUTING.md](CONTRIBUTING.md#all-local-state-lives-in-one-portable-folder)) - a
double-clicked console build's window normally closes before you can read
the error. Two ways to see it: run the `.exe` from an already-open
terminal instead of double-clicking, or check
`%TEMP%\cloudup-startup-error.log`, which any fatal startup error is
always also written to, console or not (including the `-H=windowsgui`
build, which has no console to print to at all). **Don't work around this
by running elevated/as Administrator** - move the folder instead: Windows
won't show a system tray icon for an elevated process in a normal desktop
session, so you'd have a running process with no tray icon and no way to
close it except an elevated Task Manager.

GUI mode (tray + auto-opened browser) is auto-detected from the
environment when `-gui` isn't passed explicitly: on a bare Linux VM,
container, or systemd service (no `DISPLAY`/`WAYLAND_DISPLAY`) it starts as
a plain foreground process that logs to the console instead - the same
binary works as a desktop app or a headless service/backend deployment
without needing two builds. Pass `-gui=false` explicitly if you want to be
certain regardless of environment (e.g. a Windows service, where the
auto-detection heuristic is weaker - see `cmd/server/gui_detect_windows.go`):

```bash
go run ./cmd/server -gui=false
```

To develop the frontend with hot reload instead, run it as a separate
process against a running server — see [`frontend/src`](frontend/src) and
`api.js`'s doc comment for how it finds the server (Settings page lets you
point it at a different origin, e.g. while running `npm run dev`):

```bash
# API only, no bundled frontend, plain console, CORS allowed for Vite's origin
go run ./cmd/server -static "" -gui=false -cors-origin http://localhost:5173
cd frontend && npm run dev   # separate dev server with hot reload
```

`-cors-origin` is needed **only** here. CORS is a browser-only mechanism,
and it is off by default: on the normal single-port run the page and the API
share an origin, so the browser never performs a CORS check at all, and a
non-browser client (curl, another backend calling this service) neither
consults nor needs those headers. Note that CORS was never what protects
this API — a hostile page can always *send* a request to a loopback port;
the same-origin policy only stops it from *reading* the reply. The bearer
token is the protection.

### Optional: running headless with Docker

Everything above covers the primary way to use cloudup: as a desktop app
on your own machine. Running it headless as a server is an additional,
optional capability, and [`Dockerfile`](Dockerfile)/
[`docker-compose.yml`](docker-compose.yml) are provided purely as a
**worked example** of that — a starting point to adapt, not a maintained
deployment target.

```bash
docker compose up -d --build
```

This builds a headless server image (multi-stage: `frontend/` with Node,
the Go binary, then a slim runtime). **One real caveat**: `internal/secrets`
requires an OS keychain (Windows Credential Manager / macOS Keychain /
Linux Secret Service - see
[CONTRIBUTING.md](CONTRIBUTING.md#all-local-state-lives-in-one-portable-folder)),
which a bare container doesn't have, and every provider needs at least one secret field
- with no keychain, no connection could be created at all.
[`docker/entrypoint.sh`](docker/entrypoint.sh) works around this by
starting a private D-Bus session and an auto-unlocked `gnome-keyring`
inside the container. This setup has **not been build-tested against a
live Docker daemon in this repo's own CI** - treat it as a starting point,
not a guarantee. The container also binds `0.0.0.0` instead of the usual
`127.0.0.1`-only default, since Docker's port publishing can't reach a
process listening only on the container's loopback; the bearer token is
still what actually protects the API either way (same reasoning as the
CORS note above), so set `CLOUDUP_API_TOKEN` in `docker-compose.yml` to a
real random value rather than the placeholder.

### Authorizing Google Drive, Dropbox, Yandex.Disk and OneDrive

These four need a one-time interactive OAuth step before a connection can
be used (see
[CONTRIBUTING.md](CONTRIBUTING.md#oauth2-authorization-lives-outside-the-provider-interface)
for why). The OAuth Client
ID/Secret are an app-wide setting **per provider type** — not typed per
connection — obtained once from an app you register with each provider.

**The one thing every provider needs**: register cloudup's own callback
endpoint, `<your cloudup base URL>/api/v1/oauth/callback`, as the app's
**exact** redirect URI — e.g. `http://127.0.0.1:3000/api/v1/oauth/callback`
for the default local run, or your real public URL (behind a reverse
proxy) for a remote deployment. This is a fixed, known-ahead-of-time URL —
cloudup builds it itself from whatever address your own browser is using
to reach the API right now, so it's automatically correct for both a
desktop run and a remote server, and only ever needs registering once per
provider.

- **[Google Cloud Console](https://console.cloud.google.com/apis/credentials)**:
  enable the Drive API first, then create an OAuth client of type **"Web
  application"** (not "Desktop app" — that type is for a different flow
  this no longer uses) and add the redirect URI under "Authorized redirect
  URIs". Scope requested: `drive.file` — access limited to files cloudup
  itself creates, not your whole Drive, so don't expect to see
  pre-existing files through it. While the app is in "Testing" publishing
  status (the default), only Google accounts you explicitly add as test
  users can authorize it — a common first-run surprise.
- **[Dropbox App Console](https://www.dropbox.com/developers/apps)**:
  create an app with **"Scoped access"**, access type **"Full Dropbox"**
  (not "App folder" — `rootPath` is a path you choose freely, not a
  sandboxed folder Dropbox picks for you). Add the redirect URI under
  Settings. On the **Permissions** tab, explicitly check: `account_info.read`,
  `files.content.write`, `files.content.read`, `files.metadata.read`,
  `files.metadata.write` — Dropbox requires each one checked there before
  it grants it, even though the app also requests them in the
  authorization request itself.
- **[oauth.yandex.ru/client/new](https://oauth.yandex.ru/client/new)**:
  check the **"Веб-сервисы" / "Web services"** platform checkbox — that's
  what reveals the Redirect URI field at all; other platform choices don't
  have a comparable browser-redirect flow. Enter the redirect URI (the
  console offers a "insert development URL" helper for a local address).
  Check permissions: `cloud_api:disk.write`, `cloud_api:disk.read`,
  `cloud_api:disk.info`.
- **[Azure App Registration](https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade)**:
  add a redirect URI under the **"Web"** platform (not "Mobile and desktop
  applications", despite cloudup being exactly that kind of app — "Web" is
  what accepts a client secret and a pre-registered exact URI, which is
  how this flow works now). One gotcha found the hard way: entering an
  `http://127.0.0.1` URI through the normal portal UI is rejected outright
  for the `http` scheme; use `http://localhost:<port>/api/v1/oauth/callback`
  for a local run instead (Azure treats `localhost`, unlike `127.0.0.1`,
  as portal-native) or your real `https://` URL for a deployment. Add the
  delegated permissions `Files.ReadWrite`, `User.Read`, `offline_access`.

In the frontend that's the OAuth clients section on the Settings page, then
the "Authorize" button next to the connection. Over the API directly:

```bash
TOKEN=<the API token>

# One-time, per provider type
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"clientId":"...","clientSecret":"..."}' \
  http://127.0.0.1:3000/api/v1/provider-types/googledrive/oauth-credentials

# Per connection: start the flow, open the returned authUrl, then poll
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/api/v1/connections/<id>/oauth/authorize
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/api/v1/connections/<id>/oauth/authorize
```

Which provider types need this at all is reported by
`GET /api/v1/provider-types` as `requiresOAuth`, so no client hardcodes the
list.

### Adding an interface language

The UI's translations are served by the backend, not baked into the
frontend build: `GET /api/v1/languages` lists what is installed and
`GET /api/v1/languages/{code}` returns that language's strings. English and
Russian ship inside the binary.

To add a language, drop one JSON file next to the executable and restart —
no rebuild of either the Go binary or the frontend:

```
cloudup-server
languages/
  de.json      # {"__name__": "Deutsch", "nav.queue": "Warteschlange", ...}
```

The file name is the language code; the reserved `__name__` key is the
language's own name for itself, which is what the picker on the Settings
page shows. A file named after a built-in language (`en.json`, `ru.json`)
overrides just the keys it contains, so you can correct a single string
without copying the whole catalog. Any key you leave out is filled in from
English at load time, so a partial translation is always safe. Use
`-languages <dir>` to read the folder from somewhere else — useful for a
server deployment where the executable's directory is read-only. Start from
[`internal/i18n/languages/en.json`](internal/i18n/languages/en.json) for
the full key list.

## Architecture and contributing

The design goal is: **adding a new storage backend should never require
touching the core.** How that holds in practice — the provider interface
model, self-registration, OAuth, package layout — plus the checklist for a
pull request now live in [`CONTRIBUTING.md`](CONTRIBUTING.md), since
that's who actually needs them. For a visual walkthrough instead — the
upload path, the queue's lane model, the dependency rule, as diagrams —
open [`docs/architecture.html`](docs/architecture.html) in a browser.

## License

[MIT](LICENSE)
