package pg

import (
	"context"
	"errors"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// brokenFS fails every Open, so fs.ReadDir(".") fails.
type brokenFS struct{}

func (brokenFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

// missingFileFS lists a migration that cannot then be read, the shape a
// mis-built embed or a racing file deletion produces.
type missingFileFS struct{}

func (missingFileFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func (missingFileFS) ReadDir(string) ([]fs.DirEntry, error) {
	return []fs.DirEntry{fakeDirEntry{name: "0001_first.sql"}}, nil
}

type fakeDirEntry struct {
	name string
	dir  bool
}

func (e fakeDirEntry) Name() string { return e.name }
func (e fakeDirEntry) IsDir() bool  { return e.dir }
func (e fakeDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("no info") }

// execCall records one statement the migrator issued.
type execCall struct {
	sql  string
	args []any
}

// fakeSession is a pgx connection that records statements instead of running
// them. Embedding nothing: it implements the whole session interface.
type fakeSession struct {
	execs    []execCall
	execErrs map[string]error

	queries  []string
	rows     *fakeRows
	queryErr error

	tx       *fakeTx
	beginErr error
}

func (s *fakeSession) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execs = append(s.execs, execCall{sql: sql, args: args})
	return pgconn.NewCommandTag("SELECT 1"), s.execErrs[sql]
}

func (s *fakeSession) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	s.queries = append(s.queries, sql)
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	return s.rows, nil
}

func (s *fakeSession) Begin(context.Context) (pgx.Tx, error) {
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	if s.tx == nil {
		s.tx = &fakeTx{}
	}
	return s.tx, nil
}

// execSQL returns just the statements issued, in order.
func (s *fakeSession) execSQL() []string {
	out := make([]string, 0, len(s.execs))
	for _, c := range s.execs {
		out = append(out, c.sql)
	}
	return out
}

// fakeRows serves a fixed column of strings. The embedded interface supplies
// the pgx.Rows methods the migrator never calls; calling one panics, which is
// the intent — it would mean the migrator grew an untested dependency.
type fakeRows struct {
	pgx.Rows

	values  []string
	i       int
	scanErr error
	err     error
	closed  bool
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.values) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != 1 {
		return errors.New("expected exactly one scan destination")
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("expected a *string scan destination")
	}
	*p = r.values[r.i-1]
	return nil
}

func (r *fakeRows) Close()     { r.closed = true }
func (r *fakeRows) Err() error { return r.err }

// fakeTx records the statements and the terminal call of a transaction.
type fakeTx struct {
	pgx.Tx

	execs     []execCall
	execErrs  map[string]error
	commitErr error
	committed bool
	rolled    int
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.execs = append(t.execs, execCall{sql: sql, args: args})
	return pgconn.NewCommandTag("INSERT 0 1"), t.execErrs[sql]
}

func (t *fakeTx) Commit(context.Context) error {
	if t.commitErr != nil {
		return t.commitErr
	}
	t.committed = true
	return nil
}

func (t *fakeTx) Rollback(context.Context) error {
	t.rolled++
	if t.committed {
		// What pgx returns for a rollback after a successful commit; the
		// deferred rollback in Apply must tolerate it.
		return pgx.ErrTxClosed
	}
	return nil
}

func (t *fakeTx) execSQL() []string {
	out := make([]string, 0, len(t.execs))
	for _, c := range t.execs {
		out = append(out, c.sql)
	}
	return out
}
