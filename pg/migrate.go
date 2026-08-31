package pg

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaMigrationsDDL creates the version table itself, outside the tracked
// set: it is step zero of every run and is never recorded as a migration.
const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// MigrateOptions configures one migration run.
type MigrateOptions struct {
	// LockName names the cluster-wide advisory lock replicas contend for.
	// Every process migrating the same database must pass the same name, and
	// two databases sharing a Postgres cluster should not: the lock is per
	// cluster, not per database. Required.
	LockName string

	// Logger receives one line per applied migration. Nil discards them.
	Logger *slog.Logger
}

// LockKey derives the pg_advisory_lock key for a lock name as FNV-1a/64 of the
// name, reinterpreted as int64. It is exported so a service migrating off a
// hardcoded key can assert the two agree before switching over.
func LockKey(name string) int64 {
	h := fnv.New64a()
	// hash.Hash.Write never returns an error.
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64())
}

// migrator is the slice of Postgres the migration runner drives. It keeps the
// ordering, locking and bookkeeping logic testable without a live database.
type migrator interface {
	// Lock blocks until this process holds the cluster-wide migration lock.
	Lock(ctx context.Context) error
	// Unlock releases it.
	Unlock(ctx context.Context) error
	// Discard throws away the underlying session so a lock that could not be
	// released dies with the connection instead of being returned to the pool.
	Discard(ctx context.Context)
	// EnsureVersionTable creates schema_migrations if it is missing.
	EnsureVersionTable(ctx context.Context) error
	// AppliedVersions returns the versions already recorded.
	AppliedVersions(ctx context.Context) (map[string]bool, error)
	// Apply runs one migration's SQL and records its version in a single
	// transaction, so a failure leaves neither behind.
	Apply(ctx context.Context, version, sql string) error
}

// migrationNames returns the .sql files in fsys in version (lexical) order.
func migrationNames(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, errors.New("no migrations found")
	}
	sort.Strings(names)
	return names, nil
}

// runMigrations applies every migration in fsys that schema_migrations does not
// already record, in version order, while holding the advisory lock. Replicas
// starting at the same time queue on the lock and then find nothing to do.
//
// A file absent from schema_migrations is re-run even if the live database
// already has it, which is how a schema applied out of band before adopting
// this runner is picked up. Migrations are therefore expected to be
// IF NOT EXISTS-guarded, so such a re-run is a no-op that only lands the
// missing tracking row.
func runMigrations(ctx context.Context, m migrator, fsys fs.FS, log *slog.Logger) error {
	names, err := migrationNames(fsys)
	if err != nil {
		return err
	}
	if err := m.Lock(ctx); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if err := m.Unlock(ctx); err != nil {
			// The lock is session-scoped: if the unlock did not land we cannot
			// know the session dropped it, so kill the session rather than let a
			// still-locked connection back into the pool.
			log.Warn("release migration lock, discarding connection", "err", err)
			m.Discard(ctx)
		}
	}()

	if err := m.EnsureVersionTable(ctx); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	applied, err := m.AppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}

	var n int
	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := m.Apply(ctx, name, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		log.Info("applied migration", "version", name)
		n++
	}
	if n == 0 {
		log.Info("schema up to date")
	}
	return nil
}

// session is the slice of a pgx connection the migrator drives. *pgxpool.Conn
// satisfies it; naming it keeps the SQL testable without a live database.
type session interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// pgMigrator runs migrations on one dedicated pooled connection: the advisory
// lock is session-scoped, so lock, apply and unlock must share a connection.
type pgMigrator struct {
	conn session
	key  int64
	// discard closes the physical connection; the pool destroys a closed
	// connection on Release instead of reusing it.
	discard func(context.Context)
}

func (p *pgMigrator) Lock(ctx context.Context) error {
	_, err := p.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", p.key)
	return err
}

func (p *pgMigrator) Unlock(ctx context.Context) error {
	_, err := p.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", p.key)
	return err
}

func (p *pgMigrator) Discard(ctx context.Context) { p.discard(ctx) }

func (p *pgMigrator) EnsureVersionTable(ctx context.Context) error {
	_, err := p.conn.Exec(ctx, schemaMigrationsDDL)
	return err
}

func (p *pgMigrator) AppliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := p.conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		// A read that died part-way must not look like a complete set, or the
		// caller would skip migrations it has not actually applied.
		return nil, err
	}
	return applied, nil
}

func (p *pgMigrator) Apply(ctx context.Context, version, sql string) error {
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Zero-arg Exec uses the simple protocol, so a multi-statement file runs.
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Migrate brings the database behind pool up to date with the .sql files at the
// root of fsys, applied in lexical filename order. It is safe to call from
// every replica at once: the run holds a cluster-wide advisory lock derived
// from opts.LockName, and replicas that queue behind the winner find the set
// already recorded and do nothing.
//
// Migrations run on one dedicated pooled connection, because the advisory lock
// is session-scoped. Each file is applied together with its schema_migrations
// row in a single transaction, so a failure part-way through leaves neither the
// half-applied file nor a tracking row that would skip it next time.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, opts MigrateOptions) error {
	if opts.LockName == "" {
		return errors.New("pg: MigrateOptions.LockName is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	return migrateSession(ctx, conn, func(ctx context.Context) { _ = conn.Conn().Close(ctx) }, fsys, opts)
}

// migrateSession runs the set on an already-acquired connection.
func migrateSession(ctx context.Context, conn session, discard func(context.Context), fsys fs.FS, opts MigrateOptions) error {
	m := &pgMigrator{conn: conn, key: LockKey(opts.LockName), discard: discard}
	if err := runMigrations(ctx, m, fsys, logger(opts.Logger)); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}
