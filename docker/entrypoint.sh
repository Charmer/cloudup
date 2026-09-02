#!/bin/sh
set -e

# internal/secrets stores every provider credential (WebDAV/S3 passwords,
# OAuth refresh tokens, ...) in the OS keychain via go-keyring - never in
# plaintext config (see README.md). On Linux that backend is the Secret
# Service D-Bus API, which simply doesn't exist in a bare container: with
# no D-Bus session and no keyring daemon, every secrets.Store.Set/Get call
# fails, and since *every* provider needs at least one secret field,
# nothing could be configured at all. This starts a private D-Bus session
# bus and a gnome-keyring daemon inside the container, unlocked with an
# empty password - fine here, since nothing outside this container's own
# filesystem can reach this bus. The keyring's storage file
# (~/.local/share/keyrings) lives under the same volume as the rest of
# cloudup's state (see docker-compose.yml), so it survives a restart.
exec dbus-run-session -- sh -c '
  eval "$(printf "" | gnome-keyring-daemon --unlock --components=secrets)"
  export GNOME_KEYRING_CONTROL
  exec /app/cloudup-server -gui=false -open-browser=false -addr 0.0.0.0:3000 "$@"
' _ "$@"
