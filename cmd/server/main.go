// Command server is cloudup's REST API - the service layer
// (internal/config, internal/secrets, internal/registry, internal/queue,
// internal/history, internal/settings) exposed over local HTTP. It listens
// on 127.0.0.1 only. By default it also serves the built frontend
// (frontend/dist, see -static) from the same port and opens it in the
// system browser on startup - single process, single origin, no CORS
// needed or served on that normal path. -static "" disables static
// serving entirely (API only), for anyone who'd rather run the frontend as
// a wholly separate process (e.g. `npm run dev`) against openapi.yaml; that
// case is the only one that needs -cors-origin, since it puts the page on a
// different origin than the API (see internal/httpapi's withCORS).
//
// By default this runs as a background GUI app: a system tray icon
// instead of a console window, left click reopens the browser, right
// click shows an Exit item - see tray.go. Whether that happens is
// controlled by -gui, which defaults to auto-detecting a graphical
// session (see hasGUISession, gui_detect_*.go) rather than a fixed
// default - so the same binary can be a desktop app when double-clicked
// and a plain foreground service when launched from systemd/Docker/a
// Windows service with no display, without needing two builds or the
// operator remembering a flag. Passing -gui explicitly always wins over
// auto-detection. Note that -gui alone does not suppress the console
// window on Windows; that additionally needs building with
// -ldflags="-H=windowsgui" (see README.md) - -gui only controls whether
// this process behaves like a tray app (icon, click handling, file
// logging) versus a normal foreground process.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"cloudup/internal/appdir"
	"cloudup/internal/config"
	"cloudup/internal/history"
	"cloudup/internal/httpapi"
	"cloudup/internal/i18n"
	"cloudup/internal/queue"
	"cloudup/internal/secrets"
	"cloudup/internal/settings"
	"cloudup/internal/watch"

	_ "cloudup/internal/providers/b2"
	_ "cloudup/internal/providers/dropbox"
	_ "cloudup/internal/providers/ftp"
	_ "cloudup/internal/providers/googledrive"
	_ "cloudup/internal/providers/onedrive"
	_ "cloudup/internal/providers/s3"
	_ "cloudup/internal/providers/sftp"
	_ "cloudup/internal/providers/webdav"
	_ "cloudup/internal/providers/yandexdisk"
)

func main() {
	if err := run(); err != nil {
		reportFatalStartupError(err)
		os.Exit(1)
	}
}

// reportFatalStartupError exists because of a real failure mode: a fatal
// error can happen before redirectLogToFile gets a chance to run (both it
// and internal/appdir.Dir(), which every other local state path goes
// through, can themselves be what's failing - e.g. no write access to the
// folder the exe is installed in, such as Program Files without
// elevation) - so stderr and the log file cloudup would normally use may
// both be unavailable. This always prints to stderr (useful when launched
// from an already-open terminal) and additionally best-effort writes the
// same message to a file under the OS temp directory, which is writable
// regardless of where the executable lives or whether its own folder is.
// On Windows specifically, if stdin looks like a real interactive console
// rather than something redirected, it waits for Enter before returning:
// double-clicking a console .exe opens a window that Windows closes the
// instant the process exits, so without this pause the error is visible
// for a fraction of a second and then gone.
func reportFatalStartupError(err error) {
	msg := fmt.Sprintf("cloudup-server: %s", err)
	fmt.Fprintln(os.Stderr, msg)

	logPath := filepath.Join(os.TempDir(), "cloudup-startup-error.log")
	entry := fmt.Sprintf("%s  %s\n", time.Now().Format(time.RFC3339), msg)
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
		f.WriteString(entry)
		f.Close()
		fmt.Fprintln(os.Stderr, "(also written to "+logPath+")")
	}

	if runtime.GOOS == "windows" {
		if info, serr := os.Stdin.Stat(); serr == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintln(os.Stderr, "\nPress Enter to close this window...")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}
}

// version is set at build time via:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/server
//
// A plain `go build`/`go run` leaves it at "dev" - handleUpdatesCheck
// (internal/httpapi/updates.go) still reaches GitHub and reports the
// latest release in that case, it just can't claim an update is available
// since there is nothing to compare "dev" against.
var version = "dev"

// idleGCPercent and softMemLimit tune the Go runtime for a process that is
// idle almost all the time (a tray app / local REST API waiting for
// requests) rather than a CPU-bound batch job: the default GOGC=100 lets
// the heap double before a collection runs, which wastes resident memory
// for a program whose live heap is normally tiny. Lowering GOGC collects
// sooner, keeping steady-state RSS down; softMemLimit is a ceiling GOMEMLIMIT
// only pushes back against once usage actually approaches it (e.g. several
// concurrent large multipart uploads, each buffering up to partSize*streams
// - see queue.DefaultMultipartPartSize/DefaultMultiThreadStreams), so it
// does not throttle normal operation, just guards against unbounded growth.
const (
	idleGCPercent   = 50
	softMemLimitMiB = 512
)

// tuneMemory applies idleGCPercent/softMemLimitMiB, but only for whichever
// of GOGC/GOMEMLIMIT the operator has not already set via environment
// variable - the Go runtime reads both automatically before main() runs,
// so an explicit setting here must never second-guess that choice.
func tuneMemory() {
	if _, explicit := os.LookupEnv("GOGC"); !explicit {
		debug.SetGCPercent(idleGCPercent)
	}
	if _, explicit := os.LookupEnv("GOMEMLIMIT"); !explicit {
		debug.SetMemoryLimit(softMemLimitMiB << 20)
	}
}

func run() error {
	tuneMemory()

	addr := flag.String("addr", "127.0.0.1:3000", "address to listen on - loopback only by default")
	openapiPath := flag.String("openapi", "openapi.yaml", "path to the OpenAPI spec served at /openapi.yaml")
	staticDir := flag.String("static", "frontend/dist", "directory of a built frontend to serve at / (empty to disable and run API-only)")
	openBrowser := flag.Bool("open-browser", true, "open the system browser to the frontend on startup (GUI mode only)")
	corsOrigin := flag.String("cors-origin", "", `browser origin allowed to call the API cross-origin, e.g. "http://localhost:5173" for a Vite dev server ("*" allows any). Empty - the default - serves no CORS headers at all, which is what the normal same-port run and any non-browser client need`)
	languagesDir := flag.String("languages", "", "directory of extra UI translation JSON files, added to the ones built into this binary (default: a \"languages\" folder next to the executable). Adding a language never needs a rebuild - see internal/i18n")
	gui := flag.Bool("gui", true, "run as a background app with a system tray icon (left click reopens the browser, right click offers Exit) instead of a headless service; when this flag is not passed explicitly, it is auto-detected from the environment (see hasGUISession) rather than defaulting to true")
	updateRepo := flag.String("update-repo", "Charmer/cloudup", `GitHub "owner/repo" that GET /api/v1/updates/check queries for the latest release - only ever called in direct response to a user clicking "Check for updates" in Settings, never on a timer or at startup. Change this if you run a fork and want update checks to point at your own releases`)
	flag.Parse()

	useGUI := *gui
	autoDetectedGUI := !flagPassed("gui")
	if autoDetectedGUI {
		useGUI = hasGUISession()
	}

	if useGUI {
		if err := redirectLogToFile(); err != nil {
			// Not fatal - a tray app with nowhere to log is still usable,
			// just harder to debug. os.Stderr is likely not attached to
			// anything meaningful when built with -H=windowsgui anyway.
			fmt.Fprintln(os.Stderr, "warning: could not open log file:", err)
		}
	} else if autoDetectedGUI {
		log.Printf("no GUI session detected - starting as a headless service (pass -gui=true to force tray/browser mode)")
	}

	configPath, err := config.DefaultPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	configStore, err := config.Open(configPath)
	if err != nil {
		return fmt.Errorf("opening config: %w", err)
	}

	historyPath, err := history.DefaultPath()
	if err != nil {
		return fmt.Errorf("resolving history path: %w", err)
	}
	historyStore, err := history.Open(historyPath)
	if err != nil {
		return fmt.Errorf("opening history: %w", err)
	}

	settingsPath, err := settings.DefaultPath()
	if err != nil {
		return fmt.Errorf("resolving settings path: %w", err)
	}
	settingsStore, err := settings.Open(settingsPath)
	if err != nil {
		return fmt.Errorf("opening settings: %w", err)
	}
	currentSettings, err := settingsStore.Get()
	if err != nil {
		return fmt.Errorf("reading settings: %w", err)
	}

	watchPath, err := watch.DefaultPath()
	if err != nil {
		return fmt.Errorf("resolving watch rules path: %w", err)
	}
	watchStore, err := watch.Open(watchPath)
	if err != nil {
		return fmt.Errorf("opening watch rules: %w", err)
	}

	secretsStore := secrets.New()

	queueMgr := queue.NewManager(historyStore, queue.DefaultRetryPolicy())
	queueMgr.SetConcurrency(currentSettings.MaxConcurrentUploadsPerProvider)
	queueMgr.SetVerifyAfterUpload(currentSettings.VerifyChecksumAfterUpload)
	queueMgr.SetMultiThreadStreams(currentSettings.MultiThreadThresholdBytes, currentSettings.MultiThreadStreams)
	queueMgr.SetUploadBandwidthLimit(currentSettings.MaxUploadBytesPerSecond)
	queueMgr.SetIdleQueueSweepInterval(time.Duration(currentSettings.IdleConnectionTimeoutMinutes) * time.Minute)

	token, err := resolveToken()
	if err != nil {
		return err
	}

	spoolDir := filepath.Join(os.TempDir(), "cloudup-uploads")

	srv := httpapi.NewServer(configStore, secretsStore, historyStore, settingsStore, queueMgr, watchStore, token, spoolDir)
	srv.Version = version
	srv.UpdateRepo = *updateRepo
	if *staticDir != "" {
		if _, statErr := os.Stat(filepath.Join(*staticDir, "index.html")); statErr != nil {
			log.Printf("warning: frontend not found at %s (run `npm run build` in frontend/) - serving API only", *staticDir)
		} else {
			srv.StaticDir = *staticDir
		}
	}
	if srv.StaticDir == "" {
		log.Printf("API token: %s", token)
	}
	srv.CORSOrigin = *corsOrigin
	if srv.CORSOrigin != "" {
		log.Printf("CORS enabled for origin %s", srv.CORSOrigin)
	}

	// A missing/unreadable languages directory is never fatal: the built-in
	// catalogs alone are a working UI, and losing the language picker is a
	// far smaller problem than refusing to start over a translation file.
	langDir := *languagesDir
	if langDir == "" {
		if d, dirErr := i18n.DefaultDir(); dirErr != nil {
			log.Printf("warning: could not resolve the default languages dir: %v", dirErr)
		} else {
			langDir = d
		}
	}
	if catalog, langErr := i18n.Load(langDir); langErr != nil {
		log.Printf("warning: loading translations from %s: %v (serving built-in languages only)", langDir, langErr)
		if fallback, fbErr := i18n.Load(""); fbErr == nil {
			srv.Languages = fallback
		}
	} else {
		srv.Languages = catalog
	}

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(*openapiPath),
	}

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			log.Println("shutting down...")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(ctx); err != nil {
				log.Printf("shutting down http server: %v", err)
			}
			if err := queueMgr.Shutdown(ctx); err != nil {
				log.Printf("shutting down queue: %v", err)
			}
			if srv.WatchEngine != nil {
				srv.WatchEngine.Shutdown()
			}
			historyStore.Close()
		})
	}

	url := fmt.Sprintf("http://%s", *addr)

	if useGUI {
		// Ctrl+C still works when run from a console (dev/testing) even in
		// GUI mode - route it through systray.Quit() so shutdown only ever
		// happens once, via onExit in runWithTray, regardless of which
		// trigger fired first.
		sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-sigCtx.Done()
			requestTrayQuit()
		}()

		log.Printf("cloudup starting on %s (GUI mode)", *addr)
		runWithTray(url, httpServer, shutdown, *openBrowser)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("cloudup listening on %s", url)
		errCh <- httpServer.ListenAndServe()
	}()

	if srv.StaticDir != "" && *openBrowser {
		go openInBrowser(url)
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown()
	}
	return nil
}

// resolveToken uses CLOUDUP_API_TOKEN if set (so a frontend/CI can rely on
// a fixed, pre-shared value across restarts), otherwise generates a fresh
// random token each startup and only ever surfaces it via the log line
// above - never written to disk, matching internal/secrets' "never on disk
// unencrypted" stance for every other credential in this app. When the
// static frontend is served (the normal path), the token additionally
// reaches the browser via staticHandler's injected
// window.__CLOUDUP_TOKEN__ - see internal/httpapi/server.go - so it's
// deliberately not also logged in that case, to avoid implying the user
// needs to copy it manually.
func resolveToken() (string, error) {
	if t := os.Getenv("CLOUDUP_API_TOKEN"); t != "" {
		return t, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating API token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// flagPassed reports whether a flag with the given name was explicitly
// passed on the command line, as opposed to just carrying its default
// value - flag.Visit only visits flags that were actually set. Used so an
// explicit -gui always overrides the auto-detected default (see run()).
func flagPassed(name string) bool {
	passed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

// redirectLogToFile points the standard logger at server.log in the same
// "data" directory as the rest of cloudup's local state (config.json,
// history.db, debug.log - see internal/appdir) instead of stderr. GUI mode
// implies there is no console a user would see stderr on anyway
// (especially once built with -ldflags="-H=windowsgui", see the package
// doc comment).
func redirectLogToFile() error {
	dir, err := appdir.Dir()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening server.log: %w", err)
	}
	log.SetOutput(f)
	return nil
}

// openInBrowser shells out to the OS's "open a URL" command - there's no
// portable stdlib way to do this. Best-effort: a failure here (headless
// environment, missing xdg-open, ...) just means the user opens the URL
// themselves, never a fatal error. Callers fire this right after starting
// httpServer.ListenAndServe in its own goroutine, before the listener is
// necessarily accepting connections yet - the short sleep gives it a head
// start so the browser's first request doesn't race a connection refused.
func openInBrowser(url string) {
	time.Sleep(300 * time.Millisecond)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not auto-open browser (open %s manually): %v", url, err)
	}
}
