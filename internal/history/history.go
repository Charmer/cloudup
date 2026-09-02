// Package history keeps a local record of every upload (what file, to
// which connection, at what remote path, with what checksum) in SQLite, so
// the user can later confirm a file is still present and unmodified. It
// depends only on provider.ExistenceChecker /
// provider.ChecksumVerifier, never on a concrete provider or on
// internal/registry: callers resolve the provider.Provider for a
// connection themselves and pass it into VerifyEntry.
package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"cloudup/internal/appdir"
	"cloudup/internal/provider"
)

// Upload status - the outcome of the upload attempt itself.
const (
	StatusSuccess   = "success"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

// Check status - the outcome of a later VerifyEntry call.
const (
	CheckOK           = "ok"
	CheckMissing      = "missing"
	CheckMismatch     = "mismatch"
	CheckUnverifiable = "unverifiable"
	CheckError        = "error"
)

const schema = `
CREATE TABLE IF NOT EXISTS upload_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    local_path        TEXT NOT NULL,
    local_size        INTEGER NOT NULL,
    provider_id       TEXT NOT NULL,
    provider_type     TEXT NOT NULL,
    remote_path       TEXT NOT NULL,
    remote_url        TEXT,
    checksum          TEXT,
    checksum_algo     TEXT,
    uploaded_at       TEXT NOT NULL,
    status            TEXT NOT NULL,
    last_checked_at   TEXT,
    last_check_status TEXT
);

CREATE INDEX IF NOT EXISTS idx_upload_log_provider_remote ON upload_log(provider_id, remote_path);
CREATE INDEX IF NOT EXISTS idx_upload_log_local_path      ON upload_log(local_path);
CREATE INDEX IF NOT EXISTS idx_upload_log_uploaded_at     ON upload_log(uploaded_at DESC, id DESC);
`

// Entry is one row of the upload log.
type Entry struct {
	ID              int64
	LocalPath       string
	LocalSize       int64
	ProviderID      string
	ProviderType    string
	RemotePath      string
	RemoteURL       string
	Checksum        string
	ChecksumAlgo    string
	UploadedAt      time.Time
	Status          string
	LastCheckedAt   *time.Time
	LastCheckStatus string
}

// Filter narrows List results. Zero-value ProviderID/Status are not
// applied (no filtering on that field).
type Filter struct {
	ProviderID string
	Status     string

	// Limit caps how many entries one List call returns. <= 0 falls back
	// to DefaultHistoryPageSize; values above MaxHistoryPageSize are
	// clamped down to it. GET /api/v1/history is the only caller a network
	// client controls, and an unbounded page would defeat the point of
	// paginating in the first place - a long-running install can
	// accumulate a very large upload_log over weeks, and returning it
	// whole on every request would mean an ever-growing full table scan
	// plus an ever-growing JSON response.
	Limit int

	// Offset skips this many matching rows (in List's newest-first order)
	// before collecting Limit of them - the usual page*pageSize a client
	// computes from a page number. Negative values are treated as 0.
	Offset int
}

// DefaultHistoryPageSize and MaxHistoryPageSize bound Filter.Limit - see
// its doc comment.
const (
	DefaultHistoryPageSize = 50
	MaxHistoryPageSize     = 200
)

// Page is one page of List's results plus Total, the number of entries
// matching Filter across every page (i.e. ignoring Limit/Offset) - a
// client needs Total to render "page X of Y" or to know when it has
// reached the last page, since the length of Entries alone cannot
// distinguish "this is the last page" from "this page happens to be full".
// Limit/Offset echo back the *effective* values List actually used (after
// defaulting/clamping Filter.Limit/Offset - see their doc comments), not
// simply the caller's raw input, so a client that requested an
// out-of-range Limit can still tell what it actually got.
type Page struct {
	Entries []Entry
	Total   int
	Limit   int
	Offset  int
}

// Store is a SQLite-backed upload log. Safe for concurrent use (delegated
// to database/sql's connection pool).
type Store struct {
	db *sql.DB
}

// DefaultPath returns the standard location for the history database: a
// "data" directory next to the running executable (see internal/appdir).
func DefaultPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", fmt.Errorf("history: %w", err)
	}
	return filepath.Join(dir, "history.db"), nil
}

// Open opens (creating if necessary) the SQLite database at path and
// ensures its schema exists.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("history: creating %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("history: opening %s: %w", path, err)
	}
	// SQLite only tolerates one writer at a time; serialize through a
	// single connection rather than fighting SQLITE_BUSY under
	// database/sql's connection pool.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("history: creating schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Record inserts a new log entry (typically right after an upload
// finishes, successfully or not) and returns its assigned ID.
func (s *Store) Record(ctx context.Context, e Entry) (int64, error) {
	if e.UploadedAt.IsZero() {
		e.UploadedAt = time.Now().UTC()
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO upload_log
			(local_path, local_size, provider_id, provider_type, remote_path, remote_url,
			 checksum, checksum_algo, uploaded_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.LocalPath, e.LocalSize, e.ProviderID, e.ProviderType, e.RemotePath, e.RemoteURL,
		e.Checksum, e.ChecksumAlgo, formatTime(e.UploadedAt), e.Status,
	)
	if err != nil {
		return 0, fmt.Errorf("history: recording entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("history: reading inserted id: %w", err)
	}
	return id, nil
}

// Get returns a single entry by ID.
func (s *Store) Get(ctx context.Context, id int64) (Entry, error) {
	row := s.db.QueryRowContext(ctx, selectColumns+" FROM upload_log WHERE id = ?", id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("history: entry %d not found", id)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("history: reading entry %d: %w", id, err)
	}
	return e, nil
}

// Delete removes a single log entry by ID. It only touches this local
// journal - it never deletes anything from the remote storage itself.
// Deleting an already-absent entry is not an error, matching
// internal/secrets.Store.Delete's stance that removing something already
// gone is a no-op, not a failure.
func (s *Store) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM upload_log WHERE id = ?", id); err != nil {
		return fmt.Errorf("history: deleting entry %d: %w", id, err)
	}
	return nil
}

// List returns one page of upload_log entries matching filter, newest
// first, along with the total number of matching entries - see Page and
// Filter.Limit/Offset's doc comments for why both a page and a total are
// needed.
func (s *Store) List(ctx context.Context, filter Filter) (Page, error) {
	where, args := filter.whereClause()

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_log"+where, args...).Scan(&total); err != nil {
		return Page{}, fmt.Errorf("history: counting entries: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultHistoryPageSize
	} else if limit > MaxHistoryPageSize {
		limit = MaxHistoryPageSize
	}
	offset := max(filter.Offset, 0)

	query := selectColumns + " FROM upload_log" + where + " ORDER BY uploaded_at DESC, id DESC LIMIT ? OFFSET ?"
	rows, err := s.db.QueryContext(ctx, query, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return Page{}, fmt.Errorf("history: listing entries: %w", err)
	}
	defer rows.Close()

	entries := make([]Entry, 0, limit)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return Page{}, fmt.Errorf("history: scanning entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("history: listing entries: %w", err)
	}
	return Page{Entries: entries, Total: total, Limit: limit, Offset: offset}, nil
}

// whereClause builds the " WHERE ..." SQL fragment (plus its bind args)
// shared by List's count query and page query, so the two can never drift
// out of sync with each other.
func (f Filter) whereClause() (string, []any) {
	where := " WHERE 1=1"
	var args []any
	if f.ProviderID != "" {
		where += " AND provider_id = ?"
		args = append(args, f.ProviderID)
	}
	if f.Status != "" {
		where += " AND status = ?"
		args = append(args, f.Status)
	}
	return where, args
}

// VerifyEntry re-checks a previously uploaded object against its provider:
//   - if the provider can check existence and the object is gone -> CheckMissing
//   - else if the provider can verify the stored checksum and it doesn't match -> CheckMismatch
//   - else if a checksum verification succeeded -> CheckOK
//   - if the provider supports neither check -> CheckUnverifiable (not an error)
//   - a technical failure talking to the provider -> CheckError
//
// The caller is responsible for resolving p (e.g. via internal/registry +
// internal/config + internal/secrets) for e.ProviderID; history itself does
// not know how to construct providers.
func (s *Store) VerifyEntry(ctx context.Context, e Entry, p provider.Provider) (Entry, error) {
	status := checkStatus(ctx, e, p)
	return s.setCheckStatus(ctx, e.ID, status)
}

func checkStatus(ctx context.Context, e Entry, p provider.Provider) string {
	if ec, ok := p.(provider.ExistenceChecker); ok {
		exists, err := ec.Exists(ctx, e.RemotePath)
		if err != nil {
			return CheckError
		}
		if !exists {
			return CheckMissing
		}
	}

	if cv, ok := p.(provider.ChecksumVerifier); ok && e.Checksum != "" {
		match, err := cv.VerifyChecksum(ctx, e.RemotePath, e.ChecksumAlgo, e.Checksum)
		if err != nil {
			return CheckError
		}
		if !match {
			return CheckMismatch
		}
		return CheckOK
	}

	return CheckUnverifiable
}

func (s *Store) setCheckStatus(ctx context.Context, id int64, status string) (Entry, error) {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE upload_log SET last_checked_at = ?, last_check_status = ? WHERE id = ?`,
		formatTime(now), status, id,
	)
	if err != nil {
		return Entry{}, fmt.Errorf("history: updating check status for %d: %w", id, err)
	}
	return s.Get(ctx, id)
}

const selectColumns = `SELECT id, local_path, local_size, provider_id, provider_type, remote_path, remote_url,
	checksum, checksum_algo, uploaded_at, status, last_checked_at, last_check_status`

type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(row scanner) (Entry, error) {
	var e Entry
	var remoteURL, checksum, checksumAlgo, lastCheckedAt, lastCheckStatus sql.NullString
	var uploadedAt string

	err := row.Scan(
		&e.ID, &e.LocalPath, &e.LocalSize, &e.ProviderID, &e.ProviderType, &e.RemotePath, &remoteURL,
		&checksum, &checksumAlgo, &uploadedAt, &e.Status, &lastCheckedAt, &lastCheckStatus,
	)
	if err != nil {
		return Entry{}, err
	}

	e.RemoteURL = remoteURL.String
	e.Checksum = checksum.String
	e.ChecksumAlgo = checksumAlgo.String
	e.LastCheckStatus = lastCheckStatus.String

	e.UploadedAt, err = parseTime(uploadedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("parsing uploaded_at: %w", err)
	}

	if lastCheckedAt.Valid {
		t, err := parseTime(lastCheckedAt.String)
		if err != nil {
			return Entry{}, fmt.Errorf("parsing last_checked_at: %w", err)
		}
		e.LastCheckedAt = &t
	}

	return e, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
