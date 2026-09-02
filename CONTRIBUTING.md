# Contributing to Cloud Uploader

*[Читать по-русски](CONTRIBUTING.ru.md)*

Contributions are welcome. This document covers the architecture you'll
run into while making a change, and the checklist to run through before
opening a pull request.

## How to propose a change

You don't have write access to this repository — nobody but the
maintainer does, by design. Fork it, branch, commit, and open a pull
request back to `master`; [`.github/workflows/ci.yml`](.github/workflows/ci.yml)
runs `gofmt`/`go vet`/`go test`/the frontend build on every PR, so you'll
see green/red before anyone reads the diff by hand.

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
cd frontend && npm run build
```

Run all four locally before pushing — same gate as CI, just faster to
find out about.

## Architecture

> For a visual walkthrough — the upload path, the queue's lane model and
> the dependency rule, as diagrams — open
> [`docs/architecture.html`](docs/architecture.html) in a browser. It is a
> single self-contained file; the text below covers the same ground in
> prose.

The design goal is: **adding a new storage backend should never require
touching the core.** A few principles make that hold in practice.

### Composition of small interfaces, not one large one

Every provider implements one small, mandatory interface:

```go
type Provider interface {
    Type() string
    DisplayName() string
    TestConnection(ctx context.Context) error
    Upload(ctx context.Context, task UploadTask) (UploadResult, error)
    Download(ctx context.Context, task DownloadTask) error
    List(ctx context.Context, remotePath string) ([]RemoteEntry, error)
    Delete(ctx context.Context, remotePath string) error
}
```

Anything not every backend can do — multipart upload, uploading a large
file's parts concurrently across several connections, verifying a
checksum, checking a file still exists, reporting quota, describing its
own config form — is a separate, optional interface
(`internal/provider/features.go`). The core detects support via a type
assertion (`p.(provider.ChecksumVerifier)`) and falls back gracefully when
a provider doesn't implement it. No provider is forced to stub out methods
it can't meaningfully support: Dropbox's `upload_session` API requires
each chunk's byte offset to exactly match what the server already
received, so it simply never implements `ParallelMultipartUploader` at
all — that's a protocol constraint, not a missing feature.

### Self-registering providers

Each provider package registers itself in its own `init()` — up to three
separate calls, one per concern the core needs to know about, none of
which require the core to import the provider package:

```go
// internal/providers/s3/s3.go
func init() {
    registry.Register(Type, New)                 // the Provider factory
    registry.RegisterSchema(Type, ConfigFields)   // its connection form (see below)
    // registry.RegisterOAuth(Type, oauthFlow())  // only if it needs interactive auth
}
```

The core (`internal/registry`) never imports a concrete provider package
and contains no `switch` over provider types. A new backend is a new
package plus a blank import (`import _ "cloudup/internal/providers/<name>"`)
in `cmd/server/main.go` — nothing else changes.

### A provider's connection form is data, not frontend code

Every provider's `ConfigFields() []provider.FieldSpec` (the
`provider.ConfigSchema` interface) describes its own connection form —
`{Key, Label, Type (text/password/select), Required, Options}` per field,
e.g. WebDAV's (`internal/providers/webdav/webdav.go`):

```go
func (p *Provider) ConfigFields() []provider.FieldSpec {
    return []provider.FieldSpec{
        {Key: "url", Label: "Server URL", Type: provider.FieldText, Required: true},
        {Key: secretUsername, Label: "Username", Type: provider.FieldPassword, Required: true},
        {Key: secretPassword, Label: "Password", Type: provider.FieldPassword, Required: true},
    }
}
```

`GET /api/v1/provider-types/{type}/schema` (`internal/httpapi/connections.go`)
relays this straight from `registry.Schema(type)` as JSON — the REST layer
never sees a concrete field list, just whatever the provider declared.
`ConnectionsView.vue` renders the form by iterating that array
(`v-for="f in schema"`, deciding the input type from `f.Type`) — zero
per-provider code in the frontend. On submit, the same generic split
happens client-side: anything with `Type === 'password'` goes to
`secrets{}` in the request body, everything else to `fields{}` — the
backend stores `fields` in `config.json` and routes `secrets` into
`internal/secrets` (the OS keychain), neither one hardcoding which keys to
expect for which provider.

### Every provider decides its own checksum strategy

WebDAV servers, most S3-compatible storages and Dropbox don't reliably
expose a trustworthy content hash, so those providers compute their own
SHA-256 while streaming the upload, and verify later by re-downloading and
re-hashing. Google Drive, Backblaze B2 and Yandex.Disk, on the other hand,
reliably return a native hash in their file metadata (`md5Checksum`, SHA1
and `md5` respectively), so their `VerifyChecksum` is a cheap metadata call
instead. The core doesn't know or care which strategy a given provider
uses — it only calls the shared `ChecksumVerifier` interface. Only
officially documented metadata fields are ever trusted for this — see
`internal/providers/yandexdisk`'s package doc comment for a case where a
third-party SDK listed an extra field the official API reference never
actually documents.

The user-facing "verify checksum after upload" setting
(`Settings.VerifyChecksumAfterUpload`) is a plain `map[string]bool` keyed by
provider type — the backend stores and applies it without knowing or caring
which types are cheap vs. expensive to verify. That classification (the same
WebDAV/S3/Dropbox re-download vs. Drive/B2/Yandex.Disk metadata-call split
described above) is deliberately kept out of Go entirely and instead lives as
a small hardcoded `expensiveVerifyTypes` array in `SettingsView.vue`, shown
next to each checkbox as a plain hint string. Keep that array and this
section in sync when a provider's checksum strategy changes or a new
provider is added — there is no registry-level marker to keep them honest
automatically, by design.

### OAuth2 authorization lives outside the provider interface

A Google Drive/Dropbox/Yandex.Disk/OneDrive connection can't be
constructed from stored config and secrets on first use — it first needs
an interactive consent step (open a browser, the user approves, the
provider redirects back with a code). That's exposed as a standalone pair
of functions per provider (`AuthURL`/`Exchange`, see
`internal/provider.OAuthFlow`, registered the same self-registering way as
the factory and schema above), called by the connection-setup API, instead
of being shoehorned into `ConfigSchema` (which only describes static form
fields, not multi-step interactive flows).

The redirect target is `GET /api/v1/oauth/callback` on this same REST API
server — not a separate temporary process. Earlier this used the RFC 8252
"loopback redirect" pattern native/CLI apps use (a throwaway HTTP listener
on `127.0.0.1:<random port>`), which broke two ways once cloudup grew real
deployment modes: several providers (Dropbox confirmed, Yandex likely)
require an exact, pre-registered redirect URI including the port, so a
fresh random port every attempt could never match; and a random
`127.0.0.1` port only means anything on the machine that opened it, which
isn't the machine running `cloudup-server` at all when the operator drives
a remote/headless deployment's web UI from their own laptop. Since
`cloudup-server` already runs a permanent HTTP server — unlike a typical
native app — the redirect URI now points back at *that*, built from
whatever address the browser is already using to reach the API
(`internal/httpapi`'s `requestBaseURL`, honoring `X-Forwarded-Proto`/
`-Host` behind a reverse proxy). Fixed per server, and reachable by
construction.

### Progress is polled, not pushed

`GET /api/v1/tasks` returns the current state of every upload task; the
frontend polls it on an interval instead of holding a WebSocket/SSE
connection open. Simpler on both ends, and fits a REST API meant to be
consumed by frontends other than the bundled one just as well.

### All local state lives in one portable folder

`internal/appdir.Dir()` resolves to a `data/` directory next to the
running executable — not the OS-standard per-user config location
(`os.UserConfigDir()`, e.g. `%APPDATA%`/`~/Library/Application
Support`/`~/.config`) that a typical desktop app would use.
`internal/config`, `internal/history`, `internal/settings` and
`internal/watch` all resolve their default file through it, as does
`cmd/server`'s log file in GUI mode. The point: a downloaded release
archive (or a folder you build yourself) is the *entire* install — move
it, copy it, run two of them side by side — and nothing it does ever
touches a shared, OS-managed location outside that folder. The one
deliberate exception is `internal/secrets`, which still uses the real OS
keychain: unlike the other stores, its whole reason to exist is to put
credentials somewhere *more* protected than a plain file in this folder,
not somewhere more portable.

Note for local development: `go run` compiles to a throwaway binary in a
temp directory that's deleted on exit, so state made through `go run`
doesn't persist across runs — build a real binary
(`go build -o cloudup-server ./cmd/server`) for anything that needs to
survive a restart.

### Package layout

```
internal/
  provider/            Core interfaces (Provider + optional feature interfaces), SecretStore.
  appdir/              Resolves the one "data/" folder every local state file lives under.
  registry/             Provider factory registration/lookup by type name.
  providers/
    webdav/             WebDAV (Nextcloud, ownCloud, box.com and similar).
    s3/                 AWS S3 + S3-compatible storage, with MultipartUploader + ParallelMultipartUploader.
    googledrive/        Google Drive API v3, OAuth2.
    dropbox/            Dropbox API v2, OAuth2 (app-wide client credentials).
    b2/                 Backblaze B2 native API, streaming upload with trailing SHA1, ParallelMultipartUploader.
    yandexdisk/         Yandex.Disk REST API v1, OAuth2, native MD5 metadata checksum.
    onedrive/           Microsoft Graph API v1.0, OAuth2, native quickXorHash metadata checksum.
  streamio/             Shared progress-reporting io.Reader/io.Writer wrappers.
  secrets/              OS keychain-backed SecretStore implementation.
  config/               JSON connection config store (non-secret fields only).
  history/               SQLite-backed upload log + later re-verification.
  queue/                 Upload scheduler: parallel across providers, sequential within one, retry/pause/cancel.
  watch/                 Watches a local file/folder (fsnotify) and enqueues changes via the same queue.Manager.
  settings/              JSON-backed app-wide preferences (concurrency, verify-after-upload, ...).
  httpapi/                REST API layer over all of the above - see openapi.yaml for the contract.
  debuglog/              Opt-in (CLOUDUP_DEBUG=1) HTTP traffic logging for diagnosing provider issues.
  oauthflow/             Shared OAuth2 authorization-code mechanics (AuthURL/Exchange) for OAuth providers.
  updatecheck/           On-demand GitHub release lookup for the "Check for updates" button - never automatic.
  i18n/                   UI translation catalogs: embedded defaults + drop-in external JSON files.
cmd/
  server/                 REST API + (by default) the built frontend, single process/port.
frontend/                Vue 3 + Vite reference frontend, consuming openapi.yaml over HTTP.
```

### Concurrency model

One queue per connected storage; queues for different storages run fully
in parallel. Within a single storage, uploads start in FIFO order, up to a
configurable concurrency limit (Settings → "Max concurrent uploads per
connection", default 1 — strictly sequential, matching how most APIs
behave better under a single in-flight request per account than under
heavy internal parallelism).

## Checklist for a pull request

- Tests live next to the code they test (`<file>_test.go` in the same
  package) — standard Go convention, not a separate `tests/` tree.
- `gofmt -l .`, `go vet ./...` and `go test ./... -count=1` (ideally
  `-count=3` — it catches global-state leaks between test runs that
  `-count=1` alone misses) all pass. CI runs the same checks on your PR.
- Adding a new storage provider: implement `provider.Provider` plus
  whichever optional interfaces make sense for that backend, and register
  it via `init()` — see `internal/providers/webdav` for the simplest
  example to copy from.
- Keep secrets out of `internal/config` — anything sensitive belongs in
  `internal/secrets`.
- Changing a public `internal/httpapi` endpoint means updating
  `openapi.yaml` in the same change.

## License

Contributions are accepted under the project's [MIT license](LICENSE).
