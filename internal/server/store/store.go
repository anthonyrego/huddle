// Package store is the server's Postgres: meetings, the versioned board,
// the transcript and the whiteboard's session state.
//
// Board versions are immutable and numbered from 1. A commit names the
// head it was built on (expectHead); if another commit got there first it
// fails with ErrHeadMoved and the caller re-reads and retries.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrHeadMoved means a commit lost the race for its version number.
var ErrHeadMoved = errors.New("store: the board moved on; re-read the head")

// ErrNotFound means there is no such row.
var ErrNotFound = errors.New("store: not found")

// Store is a pool of connections to the server's database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to url (a postgres:// URL) and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// migrateLock is the advisory lock that keeps two servers from migrating
// at once.
const migrateLock = 0x68756464 // "hudd"

// Migrate applies the embedded migrations that haven't run, in order, each
// in its own transaction. It reports the versions it applied.
func (s *Store) Migrate(ctx context.Context) ([]int, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLock)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	done := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		done[v] = true
	}
	rows.Close()
	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	var applied []int
	for _, f := range files {
		if done[f.version] {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + f.name)
		if err != nil {
			return applied, err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s: %w", f.name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, f.version)
			return err
		})
		if err != nil {
			return applied, err
		}
		applied = append(applied, f.version)
	}
	return applied, nil
}

type migrationFile struct {
	version int
	name    string
}

// migrationFiles lists the embedded migrations by number. Names are
// NNNN_what.sql; numbers must be unique.
func migrationFiles() ([]migrationFile, error) {
	ents, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migrationFile
	seen := map[int]string{}
	for _, e := range ents {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("store: migration %q isn't NNNN_name.sql", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share number %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		out = append(out, migrationFile{v, e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// Meeting is one meeting's row.
type Meeting struct {
	ID        string
	Title     string
	StartedAt time.Time // the meeting clock's zero
}

// EnsureMeeting returns the meeting with this id, creating it (its clock
// starting now) if it doesn't exist. Stage 1's dev rooms come into being
// this way.
func (s *Store) EnsureMeeting(ctx context.Context, id string) (Meeting, error) {
	var m Meeting
	err := s.pool.QueryRow(ctx, `
		INSERT INTO meetings (id) VALUES ($1)
		ON CONFLICT (id) DO UPDATE SET id = EXCLUDED.id
		RETURNING id, title, started_at`, id).Scan(&m.ID, &m.Title, &m.StartedAt)
	return m, err
}

// Version is one board version.
type Version struct {
	Meeting      string
	Seq, Parent  int64
	Requester    string
	Command      string
	TMs          int64
	Doc          json.RawMessage
	Layout       json.RawMessage // nil until the server lays boards out
	RestoredFrom int64           // 0 unless it restores an older version
	Ops          []json.RawMessage
}

// Head is the meeting's newest version number, 0 for an empty board.
func (s *Store) Head(ctx context.Context, meeting string) (int64, error) {
	var head int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM board_versions WHERE meeting_id = $1`, meeting).Scan(&head)
	return head, err
}

// Commit stores v as version expectHead+1 with its ops. It fails with
// ErrHeadMoved if that version already exists.
func (s *Store) Commit(ctx context.Context, v Version, expectHead int64) (Version, error) {
	v.Parent, v.Seq = expectHead, expectHead+1
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var restored *int64
		if v.RestoredFrom > 0 {
			restored = &v.RestoredFrom
		}
		var layout any
		if len(v.Layout) > 0 {
			layout = v.Layout
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO board_versions (meeting_id, seq, parent, requester, command_id, t_ms, doc, layout, restored_from)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			v.Meeting, v.Seq, v.Parent, v.Requester, v.Command, v.TMs, v.Doc, layout, restored)
		if err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" { // unique_violation
				return ErrHeadMoved
			}
			return err
		}
		for i, op := range v.Ops {
			if _, err := tx.Exec(ctx, `INSERT INTO board_ops (meeting_id, seq, idx, op) VALUES ($1, $2, $3, $4)`,
				v.Meeting, v.Seq, i, op); err != nil {
				return err
			}
		}
		return nil
	})
	return v, err
}

// Version reads one version, without its ops.
func (s *Store) Version(ctx context.Context, meeting string, seq int64) (Version, error) {
	v := Version{Meeting: meeting}
	var restored *int64
	var layout []byte
	err := s.pool.QueryRow(ctx, `
		SELECT seq, parent, requester, command_id, t_ms, doc, layout, restored_from
		FROM board_versions WHERE meeting_id = $1 AND seq = $2`, meeting, seq).
		Scan(&v.Seq, &v.Parent, &v.Requester, &v.Command, &v.TMs, &v.Doc, &layout, &restored)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if restored != nil {
		v.RestoredFrom = *restored
	}
	if len(layout) > 0 {
		v.Layout = layout
	}
	return v, err
}

// Ops reads the ops that made a version, in order.
func (s *Store) Ops(ctx context.Context, meeting string, seq int64) ([]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT op FROM board_ops WHERE meeting_id = $1 AND seq = $2 ORDER BY idx`, meeting, seq)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (json.RawMessage, error) {
		var op json.RawMessage
		err := r.Scan(&op)
		return op, err
	})
}

// Segment is one stored line of the transcript.
type Segment struct {
	Meeting, Identity, Speaker string
	StartMs, EndMs             int64
	Text                       string
}

// AddSegment stores a finished line of the transcript.
func (s *Store) AddSegment(ctx context.Context, g Segment) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO transcript_segments (meeting_id, identity, speaker, start_ms, end_ms, text)
		VALUES ($1, $2, $3, $4, $5, $6)`, g.Meeting, g.Identity, g.Speaker, g.StartMs, g.EndMs, g.Text)
	return err
}

// Segments reads the transcript between two meeting times (ms), in order.
func (s *Store) Segments(ctx context.Context, meeting string, fromMs, toMs int64) ([]Segment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT identity, speaker, start_ms, end_ms, text FROM transcript_segments
		WHERE meeting_id = $1 AND start_ms >= $2 AND start_ms < $3 ORDER BY start_ms, id`, meeting, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Segment, error) {
		g := Segment{Meeting: meeting}
		err := r.Scan(&g.Identity, &g.Speaker, &g.StartMs, &g.EndMs, &g.Text)
		return g, err
	})
}

// AgentState is what the whiteboard keeps across restarts.
type AgentState struct {
	ResumeHandle string
	Summary      string
}

// AgentState reads the meeting's whiteboard state (zero if none).
func (s *Store) AgentState(ctx context.Context, meeting string) (AgentState, error) {
	var a AgentState
	err := s.pool.QueryRow(ctx, `SELECT resume_handle, summary FROM agent_state WHERE meeting_id = $1`, meeting).
		Scan(&a.ResumeHandle, &a.Summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentState{}, nil
	}
	return a, err
}

// SetAgentState replaces the meeting's whiteboard state.
func (s *Store) SetAgentState(ctx context.Context, meeting string, a AgentState) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agent_state (meeting_id, resume_handle, summary, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (meeting_id) DO UPDATE SET resume_handle = EXCLUDED.resume_handle,
			summary = EXCLUDED.summary, updated_at = now()`, meeting, a.ResumeHandle, a.Summary)
	return err
}
