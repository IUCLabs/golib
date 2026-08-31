package pg_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"git.unkin.net/unkin/golib/pg"
	"git.unkin.net/unkin/golib/pg/pgtest"
)

// migrations is a two-file set exercising the things the runner promises:
// multi-statement files (simple protocol) and IF NOT EXISTS re-runnability.
var migrations = fstest.MapFS{
	"0001_widgets.sql": {Data: []byte(`
CREATE TABLE IF NOT EXISTS widgets (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS widgets_name_idx ON widgets (name);
`)},
	"0002_gadgets.sql": {Data: []byte(`CREATE TABLE IF NOT EXISTS gadgets (id BIGSERIAL PRIMARY KEY);`)},
	"notes.md":         {Data: []byte("ignored")},
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// The full path against a real server: connect, migrate, and confirm the schema
// and the bookkeeping table both landed.
func TestNewMigrated_AgainstRealPostgres(t *testing.T) {
	ctx := testCtx(t)
	dsn := pgtest.MustStartPostgres(ctx, t)

	pool, err := pg.NewMigrated(ctx, dsn, migrations, pg.MigrateOptions{LockName: "golib-pg-integration"})
	if err != nil {
		t.Fatalf("NewMigrated: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, table := range []string{"widgets", "gadgets", "schema_migrations"} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s was not created", table)
		}
	}

	// The second statement of the multi-statement file must have run too.
	var idx bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'widgets_name_idx')`).Scan(&idx); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !idx {
		t.Error("the second statement of the multi-statement migration did not run")
	}

	var versions []string
	rows, err := pool.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"0001_widgets.sql", "0002_gadgets.sql"}
	if len(versions) != len(want) {
		t.Fatalf("recorded versions %v, want %v", versions, want)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("recorded versions %v, want %v", versions, want)
		}
	}
}

// Every replica calls Migrate at startup. Running it concurrently against one
// server must apply the set exactly once and leave no lock held.
func TestMigrate_ConcurrentRepliesConverge(t *testing.T) {
	ctx := testCtx(t)
	dsn := pgtest.MustStartPostgres(ctx, t)

	pool, err := pg.New(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(pool.Close)

	const replicas = 4
	errs := make(chan error, replicas)
	for range replicas {
		go func() {
			errs <- pg.Migrate(ctx, pool, migrations, pg.MigrateOptions{LockName: "golib-pg-integration"})
		}()
	}
	for range replicas {
		if err := <-errs; err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 2 {
		t.Fatalf("schema_migrations has %d rows, want 2", n)
	}

	// Nothing may still hold the migration lock once every replica has finished.
	var held bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND objid IS NOT NULL AND granted)`,
	).Scan(&held); err != nil {
		t.Fatalf("check locks: %v", err)
	}
	if held {
		t.Error("an advisory lock is still held after every replica finished")
	}
}

// DSNFromEnv's output must be something pgx can actually connect with.
func TestDSNFromEnv_ConnectsToRealPostgres(t *testing.T) {
	ctx := testCtx(t)
	dsn := pgtest.MustStartPostgres(ctx, t)

	t.Setenv("DATABASE_URL", "")
	t.Setenv("APP_DATABASE_URL", "")

	cfg, err := parseDSN(dsn)
	if err != nil {
		t.Fatalf("parse container DSN: %v", err)
	}
	t.Setenv("APP_DBHOST", cfg.host)
	t.Setenv("APP_DBPORT", cfg.port)
	t.Setenv("APP_DBUSER", cfg.user)
	t.Setenv("APP_DBPASS", cfg.pass)
	t.Setenv("APP_DBNAME", cfg.name)
	t.Setenv("APP_DBSSL", "disable")

	built, err := pg.DSNFromEnv("APP_")
	if err != nil {
		t.Fatalf("DSNFromEnv: %v", err)
	}
	pool, err := pg.New(ctx, built, nil)
	if err != nil {
		t.Fatalf("New(%q): %v", built, err)
	}
	pool.Close()
}

// dsnParts is the container DSN split back into the fields DSNFromEnv reads.
type dsnParts struct{ host, port, user, pass, name string }

func parseDSN(dsn string) (dsnParts, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsnParts{}, err
	}
	pass, _ := u.User.Password()
	return dsnParts{
		host: u.Hostname(),
		port: u.Port(),
		user: u.User.Username(),
		pass: pass,
		name: strings.TrimPrefix(u.Path, "/"),
	}, nil
}
