package watch

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"cloudup/internal/provider"
	"cloudup/internal/queue"
)

// ProviderResolver resolves a connection ID into a ready-to-use
// provider.Provider. Engine never resolves providers itself (see the
// package doc comment) - cmd/server supplies a closure around
// internal/registry.Create + internal/config + internal/secrets, the same
// path internal/httpapi/uploads.go already uses for an ordinary REST
// upload.
type ProviderResolver func(connectionID string) (provider.Provider, error)

// DefaultQuietPeriod is how long a watched path must go without a new
// filesystem event before Engine treats it as "done changing" and enqueues
// an upload. A large file being copied into a watched folder fires many
// write events while it's still incomplete; uploading on the first one
// would ship a truncated file. There is no portable, reliable "the writer
// closed this file" signal across the platforms fsnotify supports, so a
// quiet period is the same practical compromise other file-watching sync
// tools use.
const DefaultQuietPeriod = 2 * time.Second

// ruleState is Engine's live bookkeeping for one Rule - the persisted Rule
// itself plus everything needed to dispatch events and report status.
// Every field is guarded by Engine.mu; there is no separate per-rule lock
// (see Engine's doc comment on why one coarse lock is enough here).
type ruleState struct {
	rule Rule

	// watchRoot is the directory actually passed to fsnotify's Watcher:
	// rule.LocalPath itself if it's a directory, or its parent if
	// rule.LocalPath is a single file (fsnotify's own recommendation for
	// watching one file - see start's doc comment).
	watchRoot  string
	singleFile bool // true if rule.LocalPath names a file, not a directory

	status  string // "watching" or "error" - see Status
	message string // non-empty only when status == "error"

	// timers debounces per absolute file path - see DefaultQuietPeriod.
	timers map[string]*time.Timer
}

// Engine watches every enabled Rule's local path and enqueues an upload
// through queueMgr whenever a file settles after being created or written.
// A single fsnotify.Watcher and a single dispatch goroutine serve every
// rule at once; Engine.mu is the one lock protecting all of Engine's and
// every ruleState's mutable fields - watch-rule changes and filesystem
// events are both rare enough relative to actual uploads that finer-
// grained locking would only add risk of getting it wrong, not meaningful
// throughput.
type Engine struct {
	queueMgr    *queue.Manager
	resolve     ProviderResolver
	quietPeriod time.Duration

	fsw *fsnotify.Watcher

	mu       sync.Mutex
	rules    map[string]*ruleState // ruleID -> state
	dirOwner map[string]string     // absolute watched directory -> ruleID, for dispatching an event to its rule

	stop chan struct{}
	done chan struct{}
}

// NewEngine creates an Engine and starts its dispatch goroutine. Call
// AddNew/Resume for each Rule that should be watched - a fresh Engine
// watches nothing on its own. Call Shutdown when done.
func NewEngine(queueMgr *queue.Manager, resolve ProviderResolver, quietPeriod time.Duration) (*Engine, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: creating fsnotify watcher: %w", err)
	}
	if quietPeriod <= 0 {
		quietPeriod = DefaultQuietPeriod
	}
	e := &Engine{
		queueMgr:    queueMgr,
		resolve:     resolve,
		quietPeriod: quietPeriod,
		fsw:         fsw,
		rules:       make(map[string]*ruleState),
		dirOwner:    make(map[string]string),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go e.run()
	return e, nil
}

// Shutdown stops the dispatch goroutine, cancels every pending debounce
// timer, and closes the underlying fsnotify.Watcher. Uploads already
// enqueued are unaffected - they belong to queue.Manager, not Engine, from
// the moment Enqueue is called.
func (e *Engine) Shutdown() {
	close(e.stop)
	<-e.done
	e.fsw.Close()

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.rules {
		for _, t := range st.timers {
			t.Stop()
		}
	}
}

// AddNew starts watching rule and immediately enqueues every file already
// present under its local path - see the package doc comment on why a
// freshly created rule uploads existing files, unlike Resume.
func (e *Engine) AddNew(rule Rule) error {
	return e.start(rule, true)
}

// Resume starts watching rule without touching whatever is already there -
// used when cmd/server loads previously created, still-enabled rules at
// startup. Re-uploading a whole folder on every restart would be both
// wasteful and surprising.
func (e *Engine) Resume(rule Rule) error {
	return e.start(rule, false)
}

// Update reconfigures an already-watched rule (or starts watching one that
// was just enabled via PUT) - equivalent to Remove followed by Resume, so
// it never re-triggers AddNew's initial full-folder upload.
func (e *Engine) Update(rule Rule) error {
	e.Remove(rule.ID)
	if !rule.Enabled {
		return nil
	}
	return e.Resume(rule)
}

// Remove stops watching a rule. Safe to call for a rule that was never
// added (e.g. one that was created disabled) or already removed.
func (e *Engine) Remove(ruleID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removeLocked(ruleID)
}

func (e *Engine) removeLocked(ruleID string) {
	st, ok := e.rules[ruleID]
	if !ok {
		return
	}
	for _, t := range st.timers {
		t.Stop()
	}
	for dir, owner := range e.dirOwner {
		if owner == ruleID {
			e.fsw.Remove(dir) // best-effort; nothing to do if it's already gone
			delete(e.dirOwner, dir)
		}
	}
	delete(e.rules, ruleID)
}

// Status reports a rule's live state: "watching" (normal), "error" (most
// recently resolving its provider or reading its local path failed - see
// message), or ok=false if the rule isn't currently being watched at all
// (never added, or disabled).
func (e *Engine) Status(ruleID string) (status, message string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, exists := e.rules[ruleID]
	if !exists {
		return "", "", false
	}
	return st.status, st.message, true
}

// start does AddNew/Resume's shared work: figure out whether rule.LocalPath
// is a file or a directory, register the right fsnotify watches, and -
// only when initialScan is true - enqueue every file already there.
//
// Watching a single file directly is unreliable across editors: many save
// by writing a temp file and renaming it over the original, which some
// platforms report as the original path being removed rather than
// changed. fsnotify's own documentation recommends watching the parent
// directory and filtering by filename instead, which is what singleFile
// mode below does.
func (e *Engine) start(rule Rule, initialScan bool) error {
	// st is registered before the os.Stat below (not after) so that a rule
	// whose path has gone missing - most commonly Resume() at startup
	// finding a previously valid folder now deleted or an external drive
	// unmounted - still shows up via Status() as "error: <reason>" instead
	// of silently vanishing (Status treats an unregistered rule the same
	// as "never watched at all", which would be misleading here: the rule
	// is still configured, it just cannot be watched right now).
	e.mu.Lock()
	e.removeLocked(rule.ID) // idempotent: drop any previous watch for this rule first
	st := &ruleState{rule: rule, status: "watching", timers: make(map[string]*time.Timer)}
	e.rules[rule.ID] = st
	e.mu.Unlock()

	info, err := os.Stat(rule.LocalPath)
	if err != nil {
		e.setStatus(rule.ID, "error", err.Error())
		return fmt.Errorf("watch: stat %q: %w", rule.LocalPath, err)
	}

	e.mu.Lock()
	if info.IsDir() {
		st.watchRoot = rule.LocalPath
		st.singleFile = false
	} else {
		st.watchRoot = filepath.Dir(rule.LocalPath)
		st.singleFile = true
	}
	e.mu.Unlock()

	if info.IsDir() {
		if err := e.watchDirTree(rule.ID, rule.LocalPath); err != nil {
			e.setStatus(rule.ID, "error", err.Error())
			return err
		}
	} else {
		if err := e.addWatch(rule.ID, st.watchRoot); err != nil {
			e.setStatus(rule.ID, "error", err.Error())
			return err
		}
	}

	if initialScan {
		e.scanExisting(rule.ID)
	}
	return nil
}

func (e *Engine) addWatch(ruleID, dir string) error {
	if err := e.fsw.Add(dir); err != nil {
		return fmt.Errorf("watch: watching %q: %w", dir, err)
	}
	e.mu.Lock()
	e.dirOwner[dir] = ruleID
	e.mu.Unlock()
	return nil
}

// watchDirTree adds a watch for root and every subdirectory under it.
func (e *Engine) watchDirTree(ruleID, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return e.addWatch(ruleID, p)
		}
		return nil
	})
}

// scanExisting walks a rule's local path and enqueues every regular file
// found - AddNew's "upload what's already there" behavior. Pre-existing
// files are not mid-write the way a just-created file might be, so this
// skips the debounce timer and enqueues directly.
func (e *Engine) scanExisting(ruleID string) {
	e.mu.Lock()
	st, ok := e.rules[ruleID]
	e.mu.Unlock()
	if !ok {
		return
	}

	if st.singleFile {
		e.enqueueFile(ruleID, st.rule.LocalPath)
		return
	}
	filepath.WalkDir(st.rule.LocalPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		e.enqueueFile(ruleID, p)
		return nil
	})
}

// run is Engine's single dispatch goroutine - the only reader of
// e.fsw.Events()/Errors(), matching queue.Manager.Events()'s "only one
// goroutine may range over this" rule for the same reason (fan-in channels
// aren't safe to read concurrently without extra coordination nothing here
// needs otherwise).
func (e *Engine) run() {
	defer close(e.done)
	for {
		select {
		case <-e.stop:
			return
		case err, ok := <-e.fsw.Errors:
			if !ok {
				return
			}
			log.Printf("watch: fsnotify error: %v", err)
		case ev, ok := <-e.fsw.Events:
			if !ok {
				return
			}
			e.handleEvent(ev)
		}
	}
}

func (e *Engine) handleEvent(ev fsnotify.Event) {
	dir := filepath.Dir(ev.Name)
	e.mu.Lock()
	ruleID, owned := e.dirOwner[dir]
	e.mu.Unlock()
	if !owned {
		return // an event for a directory we've already stopped watching
	}

	e.mu.Lock()
	st := e.rules[ruleID]
	e.mu.Unlock()
	if st == nil {
		return
	}

	// No delete propagation (see the package doc comment's design
	// decision) - Remove/Rename never trigger an upload or a remote
	// delete, only Create/Write do.
	if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
		if ev.Has(fsnotify.Create) {
			if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() && !st.singleFile {
				// A whole subdirectory appeared (e.g. a folder was moved
				// in) - start watching it and pick up anything already
				// inside it, same as the initial scan.
				e.watchDirTree(ruleID, ev.Name)
				filepath.WalkDir(ev.Name, func(p string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return nil
					}
					e.debounce(ruleID, p)
					return nil
				})
				return
			}
		}
		if st.singleFile && filepath.Base(ev.Name) != filepath.Base(st.rule.LocalPath) {
			return // parent-directory watch sees every sibling - filter to our one file
		}
		e.debounce(ruleID, ev.Name)
	}
}

// debounce (re)starts the quiet-period timer for path - see
// DefaultQuietPeriod.
func (e *Engine) debounce(ruleID, path string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st, ok := e.rules[ruleID]
	if !ok {
		return
	}
	if t, exists := st.timers[path]; exists {
		t.Stop()
	}
	st.timers[path] = time.AfterFunc(e.quietPeriod, func() {
		e.mu.Lock()
		delete(st.timers, path)
		e.mu.Unlock()
		e.enqueueFile(ruleID, path)
	})
}

// enqueueFile resolves ruleID's provider and hands path to queue.Manager.
// A file that vanished between the debounce timer firing and this running
// (e.g. it was deleted, or was a short-lived temp file) is silently
// skipped, not an error - there is nothing to upload and nothing was ever
// promised about files that do not outlive their own quiet period.
func (e *Engine) enqueueFile(ruleID, absPath string) {
	e.mu.Lock()
	st, ok := e.rules[ruleID]
	e.mu.Unlock()
	if !ok {
		return
	}

	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		return
	}

	rel, err := filepath.Rel(st.watchRoot, absPath)
	if err != nil {
		rel = filepath.Base(absPath)
	}
	if st.singleFile {
		rel = filepath.Base(absPath)
	}
	remotePath := path.Join(st.rule.RemoteFolder, filepath.ToSlash(rel))

	p, err := e.resolve(st.rule.ConnectionID)
	if err != nil {
		e.setStatus(ruleID, "error", err.Error())
		return
	}
	e.setStatus(ruleID, "watching", "")

	e.queueMgr.Enqueue(st.rule.ConnectionID, p, queue.UploadRequest{
		LocalPath:  absPath,
		RemotePath: remotePath,
		Size:       info.Size(),
		Open:       func() (io.ReadCloser, error) { return os.Open(absPath) },
	})
}

func (e *Engine) setStatus(ruleID, status, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.rules[ruleID]; ok {
		st.status = status
		st.message = message
	}
}
