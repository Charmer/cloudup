// Package queue implements the core upload scheduler: one queue per active
// provider connection, running fully in parallel with the other providers'
// queues. Within a single provider, tasks start in FIFO order, up to
// Manager's configured concurrency ("limit on simultaneous uploads",
// default 1 - i.e. strictly sequential unless raised via SetConcurrency).
//
// The Manager never resolves providers itself (no dependency on
// internal/registry or internal/config) - callers hand it an already
// constructed provider.Provider when enqueueing, keeping this package
// testable with fakes and independent of any particular transport on top.
package queue

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"cloudup/internal/history"
	"cloudup/internal/provider"
	"cloudup/internal/streamio"

	"golang.org/x/time/rate"
)

// Manager owns one queue per provider connection and fans out Events to a
// single channel for subscribers (typically the UI).
type Manager struct {
	history *history.Store
	retry   RetryPolicy
	events  chan Event

	mu          sync.Mutex
	queues      map[string]*providerQueue
	concurrency int // applied to new queues; SetConcurrency updates existing ones too

	// paused remembers Pause/Resume for providers whose queue does not exist
	// yet. Queues are created lazily on first Enqueue, so without this a
	// client that paused a connection and only then added files would watch
	// them upload anyway - the pause would have been silently dropped. See
	// Pause and queueFor.
	paused map[string]bool

	// multipartThreshold/multipartPartSize configure the chunked-upload
	// choice made in attempt. Guarded by mu; read through
	// multipartSettings.
	multipartThreshold int64
	multipartPartSize  int64

	// multiThreadThreshold/multiThreadStreams configure the further choice
	// (also made in attempt) between a provider's ordinary sequential
	// chunked upload and a concurrent one across several streams - see
	// provider.ParallelMultipartUploader. Guarded by mu; read through
	// multiThreadSettings.
	multiThreadThreshold int64
	multiThreadStreams   int

	// uploadLimiter caps the aggregate upload byte rate across every
	// provider queue combined (a single global budget rather than a
	// per-provider one - see SetUploadBandwidthLimit). Starts at rate.Inf
	// (unlimited); *rate.Limiter is safe for concurrent
	// SetLimit/SetBurst/WaitN calls on its own, so this needs no extra
	// guarding by mu.
	uploadLimiter *rate.Limiter

	// verifyAfterUpload is keyed by provider TYPE (e.g. "webdav",
	// "yandexdisk"), not providerID/connection - see
	// internal/settings.Settings.VerifyChecksumAfterUpload's doc comment
	// for why this is per-type rather than one global switch. A plain
	// mutex, not atomic.Bool, since the value itself is now a map;
	// swapped wholesale on every SetVerifyAfterUpload call rather than
	// mutated in place, so a reader never sees a half-updated map.
	verifyMu          sync.RWMutex
	verifyAfterUpload map[string]bool

	// idleCandidates marks providerIDs whose queue was found idle (no
	// pending, no in-flight tasks) on the previous sweepIdleQueues tick -
	// see that method's doc comment for why eviction needs two consecutive
	// idle observations rather than acting on the first one. Guarded by mu.
	idleCandidates map[string]bool
	// sweepInterval is how often sweepIdleQueuesLoop runs sweepIdleQueues -
	// see SetIdleQueueSweepInterval and DefaultIdleQueueSweepInterval.
	sweepInterval time.Duration
	stopSweep     chan struct{}
	stopSweepOnce sync.Once

	wg     sync.WaitGroup
	nextID atomic.Uint64
}

// NewManager creates a Manager that records every upload outcome to h and
// applies retry to failed uploads. events is buffered so a slow/absent
// consumer does not stall uploads outright, but subscribers should still
// drain Events() promptly. Concurrency starts at 1 (strictly sequential per
// provider, the default) - call SetConcurrency to raise it.
func NewManager(h *history.Store, retry RetryPolicy) *Manager {
	m := &Manager{
		history:              h,
		retry:                retry,
		events:               make(chan Event, 256),
		queues:               make(map[string]*providerQueue),
		paused:               make(map[string]bool),
		concurrency:          1,
		multipartThreshold:   DefaultMultipartThreshold,
		multipartPartSize:    DefaultMultipartPartSize,
		multiThreadThreshold: DefaultMultiThreadThreshold,
		multiThreadStreams:   DefaultMultiThreadStreams,
		uploadLimiter:        rate.NewLimiter(rate.Inf, 0),
		idleCandidates:       make(map[string]bool),
		sweepInterval:        DefaultIdleQueueSweepInterval,
		stopSweep:            make(chan struct{}),
		verifyAfterUpload:    make(map[string]bool),
	}
	go m.sweepIdleQueuesLoop()
	return m
}

// DefaultMultipartThreshold and DefaultMultipartPartSize control when a
// provider's chunked-upload path is used, and how big each chunk is.
//
// The threshold is 64 MiB rather than something larger for a concrete
// reason: Dropbox's single-request upload endpoint refuses anything over
// 150 MB, so the switch to chunks has to happen comfortably below that or
// large uploads to Dropbox fail outright. Chunking earlier than strictly
// necessary costs a few extra round trips and nothing else.
//
// The part size is a generic hint - providers have their own constraints
// (S3 requires ≥5 MiB per part except the last; Dropbox wants multiples of
// 4 MiB and caps a request at 150 MB) and are expected to clamp it. 16 MiB
// satisfies every current provider's minimum while keeping the part count
// well under S3's 10,000-part ceiling for any realistic file.
//
// These are deliberately not exposed through internal/settings or the REST
// API: they are transfer-tuning internals, not something a user can make an
// informed choice about, unlike the concurrency limit.
const (
	DefaultMultipartThreshold int64 = 64 << 20 // 64 MiB
	DefaultMultipartPartSize  int64 = 16 << 20 // 16 MiB
)

// SetMultipart overrides the chunked-upload threshold and part size. Values
// ≤ 0 leave the corresponding setting unchanged, so a caller can adjust one
// without knowing the other. Applies to uploads started after this call.
func (m *Manager) SetMultipart(threshold, partSize int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if threshold > 0 {
		m.multipartThreshold = threshold
	}
	if partSize > 0 {
		m.multipartPartSize = partSize
	}
}

func (m *Manager) multipartSettings() (threshold, partSize int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.multipartThreshold, m.multipartPartSize
}

// DefaultMultiThreadThreshold and DefaultMultiThreadStreams control when a
// provider's concurrent-chunk upload path (provider.ParallelMultipartUploader)
// is used instead of the ordinary sequential chunked path, and how many
// streams run at once - the same defaults rclone's own --multi-thread-streams
// uses, since there is no protocol reason to pick different ones and a
// reader already familiar with rclone's numbers gets a sane starting point
// for free.
//
// Unlike DefaultMultipartThreshold/DefaultMultipartPartSize, these ARE
// exposed through internal/settings/the REST API (see
// internal/settings.Settings.MultiThreadStreams/MultiThreadThresholdBytes):
// whether to spend extra concurrent connections on one large file is a
// genuine, informed trade-off a user can make (more streams can mean more
// throughput on a fast, high-latency link, at the cost of more simultaneous
// connections to the same account/bucket), unlike the chunked-upload
// internals above, which exist purely to satisfy protocol constraints.
const (
	DefaultMultiThreadThreshold int64 = 256 << 20 // 256 MiB, rclone's default
	DefaultMultiThreadStreams   int   = 4         // rclone's default
)

// SetMultiThreadStreams overrides the multi-thread-streams threshold and
// stream count. threshold ≤ 0 leaves it unchanged; streams ≤ 0 leaves it
// unchanged (streams == 1 is a valid explicit value - attempt's dispatch
// requires streams > 1 before ever considering the parallel path, so
// setting it to 1 is how a caller disables multi-threading outright without
// touching the threshold). Applies to uploads started after this call.
func (m *Manager) SetMultiThreadStreams(threshold int64, streams int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if threshold > 0 {
		m.multiThreadThreshold = threshold
	}
	if streams > 0 {
		m.multiThreadStreams = streams
	}
}

func (m *Manager) multiThreadSettings() (threshold int64, streams int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.multiThreadThreshold, m.multiThreadStreams
}

// SetUploadBandwidthLimit caps the combined upload byte rate across every
// provider queue at bytesPerSecond - a single global budget, not a
// per-provider one, so N providers uploading in parallel still share it
// rather than each getting their own. bytesPerSecond <= 0 removes
// the cap entirely. Burst is set equal to the rate itself (one second's
// worth of allowance) so short bursts on an otherwise idle link aren't
// needlessly smoothed away. Applies immediately to uploads already in
// flight, not just ones started afterward - *rate.Limiter's own
// SetLimit/SetBurst are safe to call concurrently with WaitN.
func (m *Manager) SetUploadBandwidthLimit(bytesPerSecond int64) {
	if bytesPerSecond <= 0 {
		m.uploadLimiter.SetBurst(0)
		m.uploadLimiter.SetLimit(rate.Inf)
		return
	}
	m.uploadLimiter.SetBurst(int(bytesPerSecond))
	m.uploadLimiter.SetLimit(rate.Limit(bytesPerSecond))
}

// SetConcurrency changes how many uploads may run at once within each
// provider's queue (tasks still start in FIFO order - this only raises how
// many of the oldest pending tasks may run simultaneously). Applies to
// every existing queue immediately (waking any dispatcher currently
// blocked waiting for a free slot) and to every queue created afterward.
// n < 1 is clamped to 1, since 0 or negative would stall all queues.
func (m *Manager) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}

	m.mu.Lock()
	m.concurrency = n
	queues := make([]*providerQueue, 0, len(m.queues))
	for _, q := range m.queues {
		queues = append(queues, q)
	}
	m.mu.Unlock()

	for _, q := range queues {
		q.mu.Lock()
		q.concurrency = n
		q.mu.Unlock()
		q.cond.Broadcast()
	}
}

// SetVerifyAfterUpload turns automatic post-upload checksum verification
// on or off per provider TYPE (see finish()'s doc comment for what this
// costs on providers like webdav/s3 that lack a native hash). byType
// replaces the whole map; a type absent from it is off, matching a fresh
// install where nothing has been configured yet. Takes effect on the next
// upload to finish, no need to touch already-running queues.
func (m *Manager) SetVerifyAfterUpload(byType map[string]bool) {
	m.verifyMu.Lock()
	m.verifyAfterUpload = byType
	m.verifyMu.Unlock()
}

func (m *Manager) shouldVerifyAfterUpload(providerType string) bool {
	m.verifyMu.RLock()
	defer m.verifyMu.RUnlock()
	return m.verifyAfterUpload[providerType]
}

// Events returns the channel of queue/task activity. There is a single
// shared channel for all providers; subscribers filter by Event.ProviderID
// if they only care about one.
func (m *Manager) Events() <-chan Event {
	return m.events
}

// queuedTask is one accepted upload, pending or in flight.
type queuedTask struct {
	id     string
	req    UploadRequest
	ctx    context.Context
	cancel context.CancelFunc
}

// providerQueue is the FIFO queue and worker state for a single provider
// connection. pending/paused/closed/current/concurrency are guarded by mu;
// cond wakes the dispatcher goroutine whenever any of them change. current
// holds every task presently in flight (up to concurrency of them), keyed
// by task ID.
type providerQueue struct {
	mu          sync.Mutex
	cond        *sync.Cond
	provider    provider.Provider
	pending     []*queuedTask
	current     map[string]*queuedTask
	concurrency int
	paused      bool
	closed      bool
}

func newProviderQueue(p provider.Provider, concurrency int) *providerQueue {
	q := &providerQueue{provider: p, current: make(map[string]*queuedTask), concurrency: concurrency}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue adds req to providerID's queue, creating the queue (and its
// worker goroutine) on first use. p is the provider to upload through; if
// the queue already exists, p replaces the stored provider (e.g. after a
// credential refresh) without disturbing pending/in-flight tasks. Returns
// the assigned task ID, usable with CancelTask.
func (m *Manager) Enqueue(providerID string, p provider.Provider, req UploadRequest) string {
	q := m.queueFor(providerID, p)

	id := fmt.Sprintf("t%d", m.nextID.Add(1))
	ctx, cancel := context.WithCancel(context.Background())
	task := &queuedTask{id: id, req: req, ctx: ctx, cancel: cancel}

	q.mu.Lock()
	q.provider = p
	q.pending = append(q.pending, task)
	q.mu.Unlock()
	q.cond.Broadcast()

	m.emit(Event{ProviderID: providerID, TaskID: id, LocalPath: req.LocalPath, RemotePath: req.RemotePath, Status: StatusQueued})
	return id
}

func (m *Manager) queueFor(providerID string, p provider.Provider) *providerQueue {
	m.mu.Lock()
	defer m.mu.Unlock()

	if q, ok := m.queues[providerID]; ok {
		return q
	}
	q := newProviderQueue(p, m.concurrency)
	// Carry over a pause requested before this queue existed - see the
	// paused field's comment. Safe to touch q.paused without q.mu: q is not
	// in m.queues yet and its worker has not started, so nothing else can
	// reach it while we hold m.mu.
	q.paused = m.paused[providerID]
	m.queues[providerID] = q
	m.wg.Add(1)
	go m.runProviderQueue(providerID, q)
	return q
}

// Pause stops providerID's queue from starting new tasks once its current
// upload (if any) finishes. Pending tasks stay queued.
//
// Pausing a provider that has no queue yet is honoured too: the request is
// remembered and applied when the queue is created on first Enqueue. That
// matters because a client naturally pauses a connection *before* adding
// files to it, and queues only come into existence once something is
// enqueued.
func (m *Manager) Pause(providerID string) { m.setPaused(providerID, true) }

// Resume undoes Pause, including a pause that was recorded before the
// queue existed.
func (m *Manager) Resume(providerID string) { m.setPaused(providerID, false) }

func (m *Manager) setPaused(providerID string, paused bool) {
	m.mu.Lock()
	m.paused[providerID] = paused
	q, ok := m.queues[providerID]
	m.mu.Unlock()

	if !ok {
		return
	}
	q.mu.Lock()
	q.paused = paused
	q.mu.Unlock()
	q.cond.Broadcast()
}

// CancelTask cancels a single task by ID, whether it is still pending or
// currently uploading. Canceling an already-finished task is a no-op.
func (m *Manager) CancelTask(providerID, taskID string) {
	m.withQueue(providerID, func(q *providerQueue) {
		q.mu.Lock()
		defer q.mu.Unlock()

		if t, ok := q.current[taskID]; ok {
			t.cancel()
			return
		}
		for i, t := range q.pending {
			if t.id == taskID {
				q.pending = append(q.pending[:i], q.pending[i+1:]...)
				t.cancel()
				providerType := q.provider.Type()
				go m.finish(providerID, providerType, q.provider, t, history.StatusCancelled, provider.UploadResult{}, nil)
				return
			}
		}
	})
}

// CancelAll cancels every task queued or in flight for providerID (current
// upload included) without closing the queue - it stays usable for future
// Enqueue calls.
func (m *Manager) CancelAll(providerID string) {
	m.withQueue(providerID, func(q *providerQueue) {
		q.mu.Lock()
		pending := q.pending
		q.pending = nil
		current := make([]*queuedTask, 0, len(q.current))
		for _, t := range q.current {
			current = append(current, t)
		}
		providerType := ""
		qProvider := q.provider
		if qProvider != nil {
			providerType = qProvider.Type()
		}
		q.mu.Unlock()

		for _, t := range pending {
			t.cancel()
			go m.finish(providerID, providerType, qProvider, t, history.StatusCancelled, provider.UploadResult{}, nil)
		}
		// Only cancel the context here - process() detects ctx.Err() itself
		// and calls finish exactly once per in-flight task, so we must not
		// record any of them a second time.
		for _, t := range current {
			t.cancel()
		}
	})
}

func (m *Manager) withQueue(providerID string, fn func(*providerQueue)) {
	m.mu.Lock()
	q, ok := m.queues[providerID]
	m.mu.Unlock()
	if ok {
		fn(q)
	}
}

// Shutdown cancels every in-flight and pending task across all provider
// queues, stops their workers, and waits (bounded by ctx) for them to
// exit.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	queues := make(map[string]*providerQueue, len(m.queues))
	for id, q := range m.queues {
		queues[id] = q
	}
	m.mu.Unlock()

	for providerID, q := range queues {
		m.CancelAll(providerID)
		q.mu.Lock()
		q.closed = true
		q.mu.Unlock()
		q.cond.Broadcast()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	m.stopSweepOnce.Do(func() { close(m.stopSweep) })

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DefaultIdleQueueSweepInterval and the "two consecutive sweeps" rule in
// sweepIdleQueues bound how many stale providerQueue entries (and their
// dispatcher goroutines) Manager can accumulate over a long-running
// process's lifetime. A providerQueue is created lazily on first Enqueue
// (see queueFor) but, before this sweep existed, was never removed short of
// the whole Manager shutting down - so a connection that was used once and
// later deleted (see internal/httpapi's DELETE /api/v1/connections/{id})
// left an idle dispatcher goroutine parked forever. Each one is cheap on
// its own, but "cheap × every connection ever used, for the process's
// entire uptime" is exactly the kind of slow leak a backend embedded in
// another service for weeks needs to not have.
//
// This is also the user-configurable "idle connection timeout" (see
// internal/settings.Settings.IdleConnectionTimeoutMinutes /
// SetIdleQueueSweepInterval): a provider queue is evicted after being
// idle across two consecutive sweeps, i.e. after somewhere between one and
// two intervals of real idle time - not exactly one interval, because
// evicting on the *first* idle sighting would tear down and recreate a
// queue on every ordinary gap between files in the same upload (see
// sweepIdleQueues' doc comment for why that matters).
const DefaultIdleQueueSweepInterval = 10 * time.Minute

// idleQueueSweepIntervalFloor is the smallest interval
// SetIdleQueueSweepInterval accepts. Below it, the two-consecutive-sweeps
// eviction window shrinks to the point where a briefly idle queue between
// two files of the same upload risks being torn down and immediately
// recreated - exactly the thrashing this whole mechanism exists to avoid
// (see DefaultIdleQueueSweepInterval's doc comment).
const idleQueueSweepIntervalFloor = 1 * time.Minute

func (m *Manager) sweepIdleQueuesLoop() {
	for {
		timer := time.NewTimer(m.idleQueueSweepInterval())
		select {
		case <-timer.C:
			m.sweepIdleQueues()
		case <-m.stopSweep:
			timer.Stop()
			return
		}
	}
}

func (m *Manager) idleQueueSweepInterval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sweepInterval
}

// SetIdleQueueSweepInterval changes how often idle provider queues are
// checked for eviction (see DefaultIdleQueueSweepInterval's doc comment for
// what that means for actual idle time before eviction). Takes effect for
// the sweep currently being waited for - i.e. within one old interval, not
// instantly. Values below idleQueueSweepIntervalFloor are clamped up to it
// rather than rejected, since a mistakenly tiny value (e.g. a settings.json
// edited by hand, or a unit meant as minutes passed as nanoseconds) should
// degrade to "sweeps a bit more often than intended" rather than to
// thrashing every provider queue on every gap between files.
func (m *Manager) SetIdleQueueSweepInterval(d time.Duration) {
	if d < idleQueueSweepIntervalFloor {
		d = idleQueueSweepIntervalFloor
	}
	m.mu.Lock()
	m.sweepInterval = d
	m.mu.Unlock()
}

// sweepIdleQueues evicts provider queues that have had no pending or
// in-flight task across two consecutive sweep ticks (i.e. idle for between
// one and two idleQueueSweepInterval periods). Two observations, not one,
// deliberately give a queue that just finished a task and is about to
// receive its next one a full interval of grace before it is torn down -
// evicting on the first idle sighting would make idleCandidates pointless
// (every momentary gap between tasks would qualify) and could rebuild a
// dispatcher goroutine far more often than genuinely idle connections
// warrant. Evicting only drops the *scheduling* state (the queue and its
// goroutine); it never touches pending/in-flight tasks - those are
// entirely absent by definition of "idle", and both Enqueue and Pause/
// Resume (see queueFor/setPaused) transparently recreate a fresh queue
// (including any remembered pause) the next time this providerID is used.
func (m *Manager) sweepIdleQueues() {
	m.mu.Lock()
	defer m.mu.Unlock()

	stillIdle := make(map[string]bool, len(m.idleCandidates))
	for providerID, q := range m.queues {
		q.mu.Lock()
		idle := len(q.pending) == 0 && len(q.current) == 0
		q.mu.Unlock()

		if !idle {
			continue
		}
		if m.idleCandidates[providerID] {
			q.mu.Lock()
			q.closed = true
			q.mu.Unlock()
			q.cond.Broadcast()
			delete(m.queues, providerID)
			continue
		}
		stillIdle[providerID] = true
	}
	m.idleCandidates = stillIdle
}

// HasActiveTasks reports whether providerID currently has any pending or
// in-flight upload. A connection with no queue at all (never used, or
// already swept while genuinely idle) reports false.
func (m *Manager) HasActiveTasks(providerID string) bool {
	m.mu.Lock()
	q, ok := m.queues[providerID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) > 0 || len(q.current) > 0
}

// runProviderQueue is the dispatcher for one provider's queue: it starts
// up to q.concurrency tasks at once, each in its own goroutine, so a
// higher concurrency setting means more of the oldest pending tasks run
// simultaneously rather than changing FIFO order. It only stops pulling
// new tasks when closed - already-started ones finish (or are canceled by
// Shutdown/CancelAll) independently, tracked via their own m.wg entry.
func (m *Manager) runProviderQueue(providerID string, q *providerQueue) {
	defer m.wg.Done()

	for {
		q.mu.Lock()
		for !q.closed && (q.paused || len(q.pending) == 0 || len(q.current) >= q.concurrency) {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		task := q.pending[0]
		q.pending = q.pending[1:]
		q.current[task.id] = task
		p := q.provider
		q.mu.Unlock()

		m.wg.Go(func() {
			m.process(providerID, p, task)

			q.mu.Lock()
			delete(q.current, task.id)
			q.mu.Unlock()
			q.cond.Broadcast() // wake the dispatcher in case a slot just freed up
		})
	}
}

func (m *Manager) process(providerID string, p provider.Provider, task *queuedTask) {
	providerType := p.Type()
	throttle := newProgressThrottle(100 * time.Millisecond)

	maxAttempts := m.retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-task.ctx.Done():
			m.finish(providerID, providerType, p, task, history.StatusCancelled, provider.UploadResult{}, nil)
			return
		default:
		}

		result, err := m.attempt(providerID, p, task, throttle)
		if err == nil {
			m.finish(providerID, providerType, p, task, history.StatusSuccess, result, nil)
			return
		}
		if task.ctx.Err() != nil {
			// The upload failed because the task was canceled mid-flight,
			// not a transient error - report Cancelled, don't retry.
			m.finish(providerID, providerType, p, task, history.StatusCancelled, provider.UploadResult{}, nil)
			return
		}
		lastErr = err

		if attempt == maxAttempts {
			break
		}
		select {
		case <-time.After(backoffDelay(attempt, m.retry.BaseDelay, m.retry.MaxDelay)):
		case <-task.ctx.Done():
			m.finish(providerID, providerType, p, task, history.StatusCancelled, provider.UploadResult{}, nil)
			return
		}
	}

	m.finish(providerID, providerType, p, task, history.StatusError, provider.UploadResult{}, lastErr)
}

func (m *Manager) attempt(providerID string, p provider.Provider, task *queuedTask, throttle *progressThrottle) (provider.UploadResult, error) {
	req := task.req
	reader, err := req.Open()
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("opening %s: %w", req.LocalPath, err)
	}
	defer reader.Close()

	m.emit(Event{ProviderID: providerID, TaskID: task.id, LocalPath: req.LocalPath, RemotePath: req.RemotePath, Status: StatusUploading, Total: req.Size})

	uploadTask := provider.UploadTask{
		LocalPath:  req.LocalPath,
		RemotePath: req.RemotePath,
		Size:       req.Size,
		Reader:     &streamio.LimitedReader{R: reader, Limiter: m.uploadLimiter, Ctx: task.ctx},
		Progress: func(sent, total int64) {
			if throttle.Allow(sent, total) {
				m.emit(Event{ProviderID: providerID, TaskID: task.id, LocalPath: req.LocalPath, RemotePath: req.RemotePath, Status: StatusUploading, Sent: sent, Total: total})
			}
		},
	}
	// Only set when the source truly supports it (the private temp file
	// internal/httpapi spools every upload to does) - see
	// provider.UploadTask.ReaderAt's doc comment for why this is required
	// before the parallel path below can even be considered. Wrapped with
	// the same uploadLimiter as Reader above so a ParallelMultipartUploader
	// path (which reads via ReaderAt directly, never through Reader) shares
	// the same global budget instead of bypassing it.
	if ra, ok := reader.(io.ReaderAt); ok {
		uploadTask.ReaderAt = &streamio.LimitedReaderAt{R: ra, Limiter: m.uploadLimiter, Ctx: task.ctx}
	}

	// Three tiers, checked in order: type assertions, never a switch on
	// provider type. A
	// provider missing an interface simply falls through to the next tier,
	// so adding one is all it takes for a provider to opt in.
	//
	// 1. Concurrent multi-stream chunked upload (provider.
	//    ParallelMultipartUploader - S3, B2) for files at/above the
	//    multi-thread threshold, when streams > 1 and the source supports
	//    random access.
	// 2. Ordinary sequential chunked upload (provider.MultipartUploader)
	//    for files above the (lower) multipart threshold. This is also
	//    what a ParallelMultipartUploader-capable provider falls back to
	//    when multi-threading isn't applicable (streams == 1, or no
	//    ReaderAt) but the file is still big enough to need chunking at
	//    all - Dropbox's single-request upload endpoint rejects anything
	//    over 150 MB outright, so for that provider this tier is what makes
	//    large files possible at all, hence a default threshold comfortably
	//    below that limit.
	// 3. The plain single-request Upload.
	mtThreshold, streams := m.multiThreadSettings()
	if uploadTask.ReaderAt != nil && streams > 1 && req.Size >= mtThreshold {
		if pu, ok := p.(provider.ParallelMultipartUploader); ok {
			_, partSize := m.multipartSettings()
			return pu.UploadMultipartParallel(task.ctx, uploadTask, partSize, streams)
		}
	}

	threshold, partSize := m.multipartSettings()
	if req.Size > threshold {
		if mp, ok := p.(provider.MultipartUploader); ok {
			return mp.UploadMultipart(task.ctx, uploadTask, partSize)
		}
	}

	return p.Upload(task.ctx, uploadTask)
}

// finish records the terminal outcome of task to history and emits the
// corresponding terminal Event. status is one of the history.Status*
// constants; queue.Status shares the same string values by design, so it
// is used directly as the Event's Status. p is only needed for the
// optional post-upload checksum verification below (see
// SetVerifyAfterUpload) - it may be nil for the Cancelled-while-pending
// call sites, which never take that path.
func (m *Manager) finish(providerID, providerType string, p provider.Provider, task *queuedTask, status string, result provider.UploadResult, uploadErr error) {
	entry := history.Entry{
		LocalPath:    task.req.LocalPath,
		LocalSize:    task.req.Size,
		ProviderID:   providerID,
		ProviderType: providerType,
		RemotePath:   task.req.RemotePath,
		RemoteURL:    result.RemoteURL,
		Checksum:     result.Checksum,
		ChecksumAlgo: result.ChecksumAlgo,
		Status:       status,
	}

	historyID, err := m.history.Record(context.Background(), entry)
	if err != nil && uploadErr == nil {
		uploadErr = fmt.Errorf("recording history: %w", err)
	}

	m.emit(Event{
		ProviderID: providerID,
		TaskID:     task.id,
		LocalPath:  task.req.LocalPath,
		RemotePath: task.req.RemotePath,
		Status:     Status(status),
		Err:        uploadErr,
		HistoryID:  historyID,
	})

	// Optional, off by default (verification is otherwise a manual
	// action; this opts a connection's uploads into running it
	// automatically - see internal/settings.Settings.VerifyChecksumAfterUpload).
	// Deliberately run in its own goroutine rather than inline: for
	// webdav/s3 this re-downloads the whole file (no reliable native
	// hash), so blocking here would hold this task's concurrency slot
	// open for a second full transfer, delaying whatever's next in this
	// provider's queue for no benefit - verification's result only ever
	// affects history, never the upload's own Success/Error outcome.
	if status == history.StatusSuccess && p != nil && m.shouldVerifyAfterUpload(providerType) {
		entry.ID = historyID
		m.wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.history.VerifyEntry(ctx, entry, p)
		})
	}
}

func (m *Manager) emit(e Event) {
	m.events <- e
}
