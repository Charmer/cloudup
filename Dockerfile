# Builds cloudup as a headless server image. See docker-compose.yml for a
# runnable example and its comments for the OS-keychain caveat this image
# works around (internal/secrets needs a Secret Service provider, which a
# bare Linux container doesn't have - see docker/entrypoint.sh).

FROM node:20-bookworm-slim AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/frontend/dist ./frontend/dist
RUN CGO_ENABLED=1 go build -ldflags "-X main.version=${VERSION:-dev}" -o /out/cloudup-server ./cmd/server

FROM debian:bookworm-slim
# dbus-x11 + gnome-keyring give internal/secrets (go-keyring's Linux
# backend, the Secret Service D-Bus API) somewhere to store provider
# passwords/OAuth tokens - see docker/entrypoint.sh for how they're
# started and auto-unlocked. ca-certificates is needed for the HTTPS calls
# every provider (and the optional update check) makes.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates dbus-x11 gnome-keyring \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/cloudup-server ./cloudup-server
COPY --from=frontend /src/frontend/dist ./frontend/dist
COPY openapi.yaml ./openapi.yaml
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 3000
ENTRYPOINT ["/entrypoint.sh"]
