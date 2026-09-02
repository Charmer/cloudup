## What and why

<!-- What does this change, and why is it needed? Link an issue if there is one. -->

## How was this tested

<!-- go test ./... output, a manual run against a real provider, a screenshot for UI changes, etc. -->

## Checklist

- [ ] `gofmt -l .` is empty, `go vet ./...` and `go test ./... -count=1` pass locally
- [ ] Changed a public `internal/httpapi` endpoint? `openapi.yaml` is updated in this same PR
- [ ] Added a new storage provider? It's registered via `init()` and wired into `cmd/server/main.go`'s blank imports
