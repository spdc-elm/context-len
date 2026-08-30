// Package catalog provides durable-local metadata storage. Raw body bytes never
// enter this catalog; artifact rows contain references to an external blob store.
package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 2

var (
	ErrNotFound      = errors.New("catalog: not found")
	ErrPinned        = errors.New("catalog: ownership unit is pinned")
	ErrInvalidCursor = errors.New("catalog: invalid cursor")
)

type Catalog struct{ db *sql.DB }

// Open opens (or creates) a WAL-mode catalog and applies migrations.
func Open(path string) (*Catalog, error) {
	if path == "" {
		return nil, fmt.Errorf("catalog: empty path")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c := &Catalog{db: db}
	if err = c.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}
func (c *Catalog) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}
func (c *Catalog) DB() *sql.DB { return c.db }
func (c *Catalog) migrate(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;"); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);`); err != nil {
		return err
	}
	var v int
	_ = c.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&v)
	if v < 1 {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmts := []string{
			`CREATE TABLE sessions (id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, pinned INTEGER NOT NULL DEFAULT 0, retention TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0, policy TEXT NOT NULL DEFAULT '', position INTEGER NOT NULL DEFAULT 0)`,
			`CREATE TABLE exchanges (id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, position INTEGER NOT NULL DEFAULT 0, protocol TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, envelope TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', response_exchange_id TEXT NOT NULL DEFAULT '', request_exchange_id TEXT NOT NULL DEFAULT '')`,
			`CREATE INDEX exchanges_session_pos ON exchanges(session_id,position)`,
			`CREATE TABLE artifact_refs (id TEXT PRIMARY KEY, exchange_id TEXT NOT NULL REFERENCES exchanges(id) ON DELETE CASCADE, stage TEXT NOT NULL, direction TEXT NOT NULL, content_type TEXT NOT NULL DEFAULT '', content_encoding TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0, sha256 TEXT NOT NULL DEFAULT '', complete INTEGER NOT NULL DEFAULT 0, storage_ref TEXT NOT NULL)`,
			`CREATE TABLE blobs (storage_ref TEXT PRIMARY KEY, sha256 TEXT NOT NULL, size INTEGER NOT NULL, path TEXT NOT NULL DEFAULT '', ref_count INTEGER NOT NULL DEFAULT 0, delete_pending INTEGER NOT NULL DEFAULT 0)`,
			`CREATE TABLE blob_relations (storage_ref TEXT NOT NULL REFERENCES blobs(storage_ref) ON DELETE CASCADE, artifact_id TEXT NOT NULL REFERENCES artifact_refs(id) ON DELETE CASCADE, PRIMARY KEY(storage_ref,artifact_id))`,
			`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL)`,
			`CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, kind TEXT NOT NULL, metadata TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, cursor TEXT NOT NULL DEFAULT '')`,
		}
		for _, s := range stmts {
			if _, err = tx.ExecContext(ctx, s); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(2,?)", now()); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	if v >= 1 && v < 2 {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, spec := range []struct {
			table, name, ddl string
		}{
			{"sessions", "state", `ALTER TABLE sessions ADD COLUMN state TEXT NOT NULL DEFAULT ''`},
			{"sessions", "revision", `ALTER TABLE sessions ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`},
			{"sessions", "policy", `ALTER TABLE sessions ADD COLUMN policy TEXT NOT NULL DEFAULT ''`},
			{"sessions", "position", `ALTER TABLE sessions ADD COLUMN position INTEGER NOT NULL DEFAULT 0`},
			{"exchanges", "envelope", `ALTER TABLE exchanges ADD COLUMN envelope TEXT NOT NULL DEFAULT ''`},
			{"exchanges", "summary", `ALTER TABLE exchanges ADD COLUMN summary TEXT NOT NULL DEFAULT ''`},
			{"exchanges", "response_exchange_id", `ALTER TABLE exchanges ADD COLUMN response_exchange_id TEXT NOT NULL DEFAULT ''`},
			{"exchanges", "request_exchange_id", `ALTER TABLE exchanges ADD COLUMN request_exchange_id TEXT NOT NULL DEFAULT ''`},
			{"events", "cursor", `ALTER TABLE events ADD COLUMN cursor TEXT NOT NULL DEFAULT ''`},
		} {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)`, spec.table, spec.name).Scan(&exists); err != nil {
				_ = tx.Rollback()
				return err
			}
			if !exists {
				if _, err = tx.ExecContext(ctx, spec.ddl); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(2,?)", now()); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return nil
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func (c *Catalog) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := c.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	if e = fn(tx); e != nil {
		_ = tx.Rollback()
		return e
	}
	return tx.Commit()
}

type Session struct {
	ID, Owner, Title, CreatedAt, UpdatedAt, Retention, State, Policy string
	Pinned                                                           bool
	Revision, Position                                               int64
}
type Exchange struct {
	ID, SessionID, Protocol, Method, Path, CreatedAt, UpdatedAt string
	Envelope, Summary, ResponseExchangeID, RequestExchangeID    string
	Position, Status                                            int
}
type ArtifactRef struct {
	ID, ExchangeID, Stage, Direction, ContentType, ContentEncoding, SHA256, StorageRef string
	Size                                                                               int64
	Complete                                                                           bool
}
type Blob struct {
	StorageRef, SHA256, Path string
	Size, RefCount           int64
	DeletePending            bool
}
type Event struct {
	ID                                           int64
	SessionID, Kind, Metadata, CreatedAt, Cursor string
}

func (c *Catalog) UpsertSnapshot(ctx context.Context, s Session, x Exchange, refs []ArtifactRef, ev Event) error {
	return c.Tx(ctx, func(tx *sql.Tx) error {
		if x.SessionID == "" {
			x.SessionID = s.ID
		}
		if ev.SessionID == "" {
			ev.SessionID = s.ID
		}
		if s.ID != "" {
			if s.CreatedAt == "" {
				s.CreatedAt = now()
			}
			if s.UpdatedAt == "" {
				s.UpdatedAt = s.CreatedAt
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,owner,title,created_at,updated_at,pinned,retention,state,revision,policy,position) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET owner=excluded.owner,title=excluded.title,updated_at=excluded.updated_at,pinned=excluded.pinned,retention=excluded.retention,state=excluded.state,revision=excluded.revision,policy=excluded.policy,position=excluded.position`, s.ID, s.Owner, s.Title, s.CreatedAt, s.UpdatedAt, s.Pinned, s.Retention, s.State, s.Revision, s.Policy, s.Position); err != nil {
				return err
			}
		}
		if err := func() error {
			if x.CreatedAt == "" {
				x.CreatedAt = now()
			}
			if x.UpdatedAt == "" {
				x.UpdatedAt = x.CreatedAt
			}
			_, e := tx.ExecContext(ctx, `INSERT INTO exchanges(id,session_id,position,protocol,method,path,status,created_at,updated_at,envelope,summary,response_exchange_id,request_exchange_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET session_id=excluded.session_id,position=excluded.position,protocol=excluded.protocol,method=excluded.method,path=excluded.path,status=excluded.status,updated_at=excluded.updated_at,envelope=excluded.envelope,summary=excluded.summary,response_exchange_id=excluded.response_exchange_id,request_exchange_id=excluded.request_exchange_id`, x.ID, x.SessionID, x.Position, x.Protocol, x.Method, x.Path, x.Status, x.CreatedAt, x.UpdatedAt, x.Envelope, x.Summary, x.ResponseExchangeID, x.RequestExchangeID)
			return e
		}(); err != nil {
			return err
		}
		for _, a := range refs {
			if a.ExchangeID == "" {
				a.ExchangeID = x.ID
			}
			// A revision may replace an artifact's storage reference. Remove
			// stale relations first so refcounts remain exact transactionally.
			if _, e := tx.ExecContext(ctx, `DELETE FROM blob_relations WHERE artifact_id=?`, a.ID); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO blobs(storage_ref,sha256,size,path,ref_count,delete_pending) VALUES(?,?,?,?,0,0) ON CONFLICT(storage_ref) DO UPDATE SET sha256=excluded.sha256,size=excluded.size`, a.StorageRef, a.SHA256, a.Size, ""); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO artifact_refs(id,exchange_id,stage,direction,content_type,content_encoding,size,sha256,complete,storage_ref) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET exchange_id=excluded.exchange_id,stage=excluded.stage,direction=excluded.direction,content_type=excluded.content_type,content_encoding=excluded.content_encoding,size=excluded.size,sha256=excluded.sha256,complete=excluded.complete,storage_ref=excluded.storage_ref`, a.ID, a.ExchangeID, a.Stage, a.Direction, a.ContentType, a.ContentEncoding, a.Size, a.SHA256, a.Complete, a.StorageRef); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO blob_relations(storage_ref,artifact_id) VALUES(?,?)`, a.StorageRef, a.ID); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `UPDATE blobs SET ref_count=(SELECT COUNT(*) FROM blob_relations WHERE storage_ref=?),delete_pending=0 WHERE storage_ref=?`, a.StorageRef, a.StorageRef); e != nil {
				return e
			}
		}
		// Recompute every blob's relation count so replaced/removed refs cannot
		// leave stale ownership metadata behind.
		if _, e := tx.ExecContext(ctx, `UPDATE blobs SET ref_count=(SELECT COUNT(*) FROM blob_relations WHERE storage_ref=blobs.storage_ref), delete_pending=CASE WHEN NOT EXISTS(SELECT 1 FROM blob_relations WHERE storage_ref=blobs.storage_ref) THEN 1 ELSE 0 END`); e != nil {
			return e
		}
		if ev.Kind != "" {
			if ev.CreatedAt == "" {
				ev.CreatedAt = now()
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO events(session_id,kind,metadata,created_at,cursor) VALUES(?,?,?,?,?)", ev.SessionID, ev.Kind, ev.Metadata, ev.CreatedAt, ev.Cursor)
			return err
		}
		return nil

	})
}

func (c *Catalog) UpsertSession(ctx context.Context, s Session) error {
	if s.CreatedAt == "" {
		s.CreatedAt = now()
	}
	if s.UpdatedAt == "" {
		s.UpdatedAt = s.CreatedAt
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO sessions(id,owner,title,created_at,updated_at,pinned,retention,state,revision,policy,position) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET owner=excluded.owner,title=excluded.title,updated_at=excluded.updated_at,pinned=excluded.pinned,retention=excluded.retention,state=excluded.state,revision=excluded.revision,policy=excluded.policy,position=excluded.position`, s.ID, s.Owner, s.Title, s.CreatedAt, s.UpdatedAt, s.Pinned, s.Retention, s.State, s.Revision, s.Policy, s.Position)
	return err
}

func (c *Catalog) GetSession(ctx context.Context, id string) (Session, error) {
	var s Session
	var p int
	e := c.db.QueryRowContext(ctx, "SELECT id,owner,title,created_at,updated_at,pinned,retention,state,revision,policy,position FROM sessions WHERE id=?", id).Scan(&s.ID, &s.Owner, &s.Title, &s.CreatedAt, &s.UpdatedAt, &p, &s.Retention, &s.State, &s.Revision, &s.Policy, &s.Position)
	if errors.Is(e, sql.ErrNoRows) {
		e = ErrNotFound
	}
	s.Pinned = p != 0
	return s, e
}
func (c *Catalog) UpsertExchange(ctx context.Context, x Exchange) error {
	if x.CreatedAt == "" {
		x.CreatedAt = now()
	}
	if x.UpdatedAt == "" {
		x.UpdatedAt = x.CreatedAt
	}
	_, e := c.db.ExecContext(ctx, `INSERT INTO exchanges(id,session_id,position,protocol,method,path,status,created_at,updated_at,envelope,summary,response_exchange_id,request_exchange_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET position=excluded.position,protocol=excluded.protocol,method=excluded.method,path=excluded.path,status=excluded.status,updated_at=excluded.updated_at,envelope=excluded.envelope,summary=excluded.summary,response_exchange_id=excluded.response_exchange_id,request_exchange_id=excluded.request_exchange_id`, x.ID, x.SessionID, x.Position, x.Protocol, x.Method, x.Path, x.Status, x.CreatedAt, x.UpdatedAt, x.Envelope, x.Summary, x.ResponseExchangeID, x.RequestExchangeID)
	return e
}
func (c *Catalog) PutArtifactRef(ctx context.Context, a ArtifactRef) error {
	return c.Tx(ctx, func(tx *sql.Tx) error {
		// Ensure the physical blob row exists before creating its FK relation.
		if _, e := tx.ExecContext(ctx, `INSERT INTO blobs(storage_ref,sha256,size,path,ref_count,delete_pending) VALUES(?,?,?, '',0,0) ON CONFLICT(storage_ref) DO UPDATE SET sha256=excluded.sha256,size=excluded.size`, a.StorageRef, a.SHA256, a.Size); e != nil {
			return e
		}
		// Revisions may move an artifact to a different physical blob.
		if _, e := tx.ExecContext(ctx, `DELETE FROM blob_relations WHERE artifact_id=?`, a.ID); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO artifact_refs(id,exchange_id,stage,direction,content_type,content_encoding,size,sha256,complete,storage_ref) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET exchange_id=excluded.exchange_id,stage=excluded.stage,direction=excluded.direction,content_type=excluded.content_type,content_encoding=excluded.content_encoding,size=excluded.size,sha256=excluded.sha256,complete=excluded.complete,storage_ref=excluded.storage_ref`, a.ID, a.ExchangeID, a.Stage, a.Direction, a.ContentType, a.ContentEncoding, a.Size, a.SHA256, a.Complete, a.StorageRef); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO blob_relations(storage_ref,artifact_id) VALUES(?,?)`, a.StorageRef, a.ID); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `UPDATE blobs SET ref_count=(SELECT COUNT(*) FROM blob_relations WHERE storage_ref=blobs.storage_ref),delete_pending=CASE WHEN NOT EXISTS(SELECT 1 FROM blob_relations WHERE storage_ref=blobs.storage_ref) THEN 1 ELSE 0 END`)
		return e
	})
}
func (c *Catalog) UpsertBlob(ctx context.Context, b Blob) error {
	_, e := c.db.ExecContext(ctx, `INSERT INTO blobs(storage_ref,sha256,size,path,ref_count,delete_pending) VALUES(?,?,?,?,?,?) ON CONFLICT(storage_ref) DO UPDATE SET path=excluded.path,delete_pending=excluded.delete_pending`, b.StorageRef, b.SHA256, b.Size, b.Path, b.RefCount, b.DeletePending)
	return e
}
func (c *Catalog) SetSetting(ctx context.Context, key, value string) error {
	_, e := c.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, now())
	return e
}
func (c *Catalog) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	e := c.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		e = ErrNotFound
	}
	return v, e
}
func (c *Catalog) AddEvent(ctx context.Context, e Event) error {
	if e.CreatedAt == "" {
		e.CreatedAt = now()
	}
	_, err := c.db.ExecContext(ctx, "INSERT INTO events(session_id,kind,metadata,created_at,cursor) VALUES(?,?,?,?,?)", e.SessionID, e.Kind, e.Metadata, e.CreatedAt, e.Cursor)
	return err
}
func (c *Catalog) SetPinned(ctx context.Context, id string, pinned bool) error {
	r, e := c.db.ExecContext(ctx, "UPDATE sessions SET pinned=?,updated_at=? WHERE id=?", pinned, now(), id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSession removes one complete ownership unit. Pinned sessions are protected.
// Blob rows that lose their last relation remain as delete_pending work for the external repository sweeper.
func (c *Catalog) DeleteSession(ctx context.Context, id string) error {
	return c.Tx(ctx, func(tx *sql.Tx) error {
		var pinned bool
		if e := tx.QueryRowContext(ctx, "SELECT pinned FROM sessions WHERE id=?", id).Scan(&pinned); errors.Is(e, sql.ErrNoRows) {
			return ErrNotFound
		} else if e != nil {
			return e
		} else if pinned {
			return ErrPinned
		}
		rows, e := tx.QueryContext(ctx, `SELECT DISTINCT br.storage_ref FROM blob_relations br JOIN artifact_refs a ON a.id=br.artifact_id JOIN exchanges x ON x.id=a.exchange_id WHERE x.session_id=?`, id)
		if e != nil {
			return e
		}
		var refs []string
		for rows.Next() {
			var r string
			if e = rows.Scan(&r); e != nil {
				rows.Close()
				return e
			}
			refs = append(refs, r)
		}
		if e = rows.Close(); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id); e != nil {
			return e
		}
		for _, r := range refs {
			if _, e = tx.ExecContext(ctx, `UPDATE blobs SET ref_count=(SELECT COUNT(*) FROM blob_relations WHERE storage_ref=?),delete_pending=CASE WHEN NOT EXISTS(SELECT 1 FROM blob_relations WHERE storage_ref=?) THEN 1 ELSE delete_pending END WHERE storage_ref=?`, r, r, r); e != nil {
				return e
			}
		}
		return nil
	})
}
func (c *Catalog) ListSessions(ctx context.Context) ([]Session, error) {
	rows, e := c.db.QueryContext(ctx, "SELECT id,owner,title,created_at,updated_at,pinned,retention,state,revision,policy,position FROM sessions ORDER BY created_at,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var p int
		if e = rows.Scan(&s.ID, &s.Owner, &s.Title, &s.CreatedAt, &s.UpdatedAt, &p, &s.Retention, &s.State, &s.Revision, &s.Policy, &s.Position); e != nil {
			return nil, e
		}
		s.Pinned = p != 0
		out = append(out, s)
	}
	return out, rows.Err()
}
func (c *Catalog) ListExchanges(ctx context.Context, sessionID string) ([]Exchange, error) {
	query := "SELECT id,session_id,position,protocol,method,path,status,created_at,updated_at,envelope,summary,response_exchange_id,request_exchange_id FROM exchanges"
	args := []any{}
	if sessionID != "" {
		query += " WHERE session_id=?"
		args = append(args, sessionID)
	}
	query += " ORDER BY created_at,id"
	rows, e := c.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Exchange
	for rows.Next() {
		var x Exchange
		if e = rows.Scan(&x.ID, &x.SessionID, &x.Position, &x.Protocol, &x.Method, &x.Path, &x.Status, &x.CreatedAt, &x.UpdatedAt, &x.Envelope, &x.Summary, &x.ResponseExchangeID, &x.RequestExchangeID); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListExchangesPage returns a bounded keyset page ordered by created_at,id. Cursor is an opaque,
// base64url-encoded created_at/id pair; callers must not depend on its shape.
func (c *Catalog) ListExchangesPage(ctx context.Context, sessionID string, limit int, cursor string) ([]Exchange, string, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	createdAt, id := "", ""
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", ErrInvalidCursor
		}
		parts := strings.SplitN(string(decoded), "|", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, "", ErrInvalidCursor
		}
		createdAt, id = parts[0], parts[1]
	}
	q := "SELECT id,session_id,position,protocol,method,path,status,created_at,updated_at,envelope,summary,response_exchange_id,request_exchange_id FROM exchanges WHERE (created_at>? OR (created_at=? AND id>?))"
	args := []any{createdAt, createdAt, id}
	if sessionID != "" {
		q += " AND session_id=?"
		args = append(args, sessionID)
	}
	q += " ORDER BY created_at,id LIMIT ?"
	args = append(args, limit+1)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]Exchange, 0, limit)
	for rows.Next() {
		var x Exchange
		if err := rows.Scan(&x.ID, &x.SessionID, &x.Position, &x.Protocol, &x.Method, &x.Path, &x.Status, &x.CreatedAt, &x.UpdatedAt, &x.Envelope, &x.Summary, &x.ResponseExchangeID, &x.RequestExchangeID); err != nil {
			return nil, "", err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) <= limit {
		return out, "", nil
	}
	last := out[limit-1]
	out = out[:limit]
	next := base64.RawURLEncoding.EncodeToString([]byte(last.CreatedAt + "|" + last.ID))
	return out, next, nil
}

// GetExchange returns one durable metadata row by id.
func (c *Catalog) GetExchange(ctx context.Context, id string) (Exchange, error) {
	var x Exchange
	err := c.db.QueryRowContext(ctx, "SELECT id,session_id,position,protocol,method,path,status,created_at,updated_at,envelope,summary,response_exchange_id,request_exchange_id FROM exchanges WHERE id=?", id).Scan(&x.ID, &x.SessionID, &x.Position, &x.Protocol, &x.Method, &x.Path, &x.Status, &x.CreatedAt, &x.UpdatedAt, &x.Envelope, &x.Summary, &x.ResponseExchangeID, &x.RequestExchangeID)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// SetResponseID persists the upstream Responses identifier as metadata.
func (c *Catalog) SetResponseID(ctx context.Context, exchangeID, responseID string) error {
	_, err := c.db.ExecContext(ctx, "UPDATE exchanges SET response_exchange_id=?,updated_at=? WHERE id=?", responseID, now(), exchangeID)
	return err
}
func (c *Catalog) ListArtifactRefs(ctx context.Context, exchangeID string) ([]ArtifactRef, error) {
	rows, e := c.db.QueryContext(ctx, "SELECT id,exchange_id,stage,direction,content_type,content_encoding,size,sha256,complete,storage_ref FROM artifact_refs WHERE exchange_id=? ORDER BY id", exchangeID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []ArtifactRef
	for rows.Next() {
		var a ArtifactRef
		var complete int
		if e = rows.Scan(&a.ID, &a.ExchangeID, &a.Stage, &a.Direction, &a.ContentType, &a.ContentEncoding, &a.Size, &a.SHA256, &complete, &a.StorageRef); e != nil {
			return nil, e
		}
		a.Complete = complete != 0
		out = append(out, a)
	}
	return out, rows.Err()
}
func (c *Catalog) PendingBlobDeletes(ctx context.Context) ([]Blob, error) {
	rows, e := c.db.QueryContext(ctx, "SELECT storage_ref,sha256,size,path,ref_count,delete_pending FROM blobs WHERE delete_pending=1 AND ref_count=0 ORDER BY storage_ref")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Blob
	for rows.Next() {
		var b Blob
		var pending int
		if e = rows.Scan(&b.StorageRef, &b.SHA256, &b.Size, &b.Path, &b.RefCount, &pending); e != nil {
			return nil, e
		}
		b.DeletePending = pending != 0
		out = append(out, b)
	}
	return out, rows.Err()
}
func (c *Catalog) MarkBlobDeleted(ctx context.Context, storageRef string) error {
	r, e := c.db.ExecContext(ctx, "DELETE FROM blobs WHERE storage_ref=? AND ref_count=0 AND delete_pending=1", storageRef)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Clear atomically removes all metadata; external blob deletion should sweep unreferenced files afterwards.
func (c *Catalog) Clear(ctx context.Context) error {
	return c.Tx(ctx, func(tx *sql.Tx) error {
		for _, t := range []string{"blob_relations", "artifact_refs", "exchanges", "events", "sessions"} {
			if _, e := tx.ExecContext(ctx, "DELETE FROM "+t); e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(ctx, "UPDATE blobs SET ref_count=0,delete_pending=1")
		return e
	})
}

// ListAllArtifactRefs returns every durable artifact reference.
func (c *Catalog) ListAllArtifactRefs(ctx context.Context) ([]ArtifactRef, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT id,exchange_id,stage,direction,content_type,content_encoding,size,sha256,complete,storage_ref FROM artifact_refs ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArtifactRef
	for rows.Next() {
		var a ArtifactRef
		var complete int
		if err := rows.Scan(&a.ID, &a.ExchangeID, &a.Stage, &a.Direction, &a.ContentType, &a.ContentEncoding, &a.Size, &a.SHA256, &complete, &a.StorageRef); err != nil {
			return nil, err
		}
		a.Complete = complete != 0
		out = append(out, a)
	}
	return out, rows.Err()
}
func (c *Catalog) ListBlobs(ctx context.Context) ([]Blob, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT storage_ref,sha256,size,path,ref_count,delete_pending FROM blobs ORDER BY storage_ref")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Blob
	for rows.Next() {
		var b Blob
		var d int
		if err := rows.Scan(&b.StorageRef, &b.SHA256, &b.Size, &b.Path, &b.RefCount, &d); err != nil {
			return nil, err
		}
		b.DeletePending = d != 0
		out = append(out, b)
	}
	return out, rows.Err()
}
