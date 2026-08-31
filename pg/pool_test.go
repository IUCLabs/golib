package pg

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// deadDSN points at a port nothing listens on, so connecting fails immediately
// and locally: no container, no network, no waiting.
const deadDSN = "postgres://u:p@127.0.0.1:1/d?sslmode=disable&connect_timeout=2"

func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNew_RejectsAnUnparseableDSN(t *testing.T) {
	pool, err := New(shortCtx(t), "://not a dsn", nil)
	if err == nil {
		pool.Close()
		t.Fatal("expected an error for an unparseable DSN")
	}
	if !strings.Contains(err.Error(), "connect postgres") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

// pgxpool connects lazily, so New must ping: without it a wrong address only
// surfaces on the first query, long after startup reported success.
func TestNew_PingsBeforeReturning(t *testing.T) {
	pool, err := New(shortCtx(t), deadDSN, nil)
	if err == nil {
		pool.Close()
		t.Fatal("expected New to fail against an unreachable server")
	}
	if !strings.Contains(err.Error(), "ping postgres") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

func TestNewMigrated_PropagatesConnectFailure(t *testing.T) {
	pool, err := NewMigrated(shortCtx(t), deadDSN, testFS(), MigrateOptions{LockName: testLockName})
	if err == nil {
		pool.Close()
		t.Fatal("expected NewMigrated to fail against an unreachable server")
	}
	if !strings.Contains(err.Error(), "ping postgres") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

// LockName is what makes replicas exclude each other; defaulting it would let a
// caller silently share a key with an unrelated service, so it is required.
func TestMigrate_RequiresALockName(t *testing.T) {
	pool, err := pgxpool.New(shortCtx(t), deadDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	err = Migrate(shortCtx(t), pool, testFS(), MigrateOptions{})
	if err == nil {
		t.Fatal("expected Migrate to reject an empty LockName")
	}
	if !strings.Contains(err.Error(), "LockName") {
		t.Fatalf("error %q does not name the missing option", err)
	}
}

func TestMigrate_ReportsAcquireFailure(t *testing.T) {
	pool, err := pgxpool.New(shortCtx(t), deadDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	err = Migrate(shortCtx(t), pool, testFS(), MigrateOptions{LockName: testLockName})
	if err == nil {
		t.Fatal("expected Migrate to fail when no connection can be acquired")
	}
	if !strings.Contains(err.Error(), "acquire migration connection") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

// An empty migration set is a build mistake, not an empty schema; it must fail
// before any connection work rather than reporting a successful no-op run.
func TestMigrateSession_EmptySetIsAnError(t *testing.T) {
	err := migrateSession(context.Background(), &fakeSession{}, func(context.Context) {},
		fstest.MapFS{}, MigrateOptions{LockName: testLockName})
	if err == nil {
		t.Fatal("expected an empty migration set to be an error")
	}
}

// A nil Logger is the common case for a library caller; it must not panic.
func TestLogger_NilIsDiscarding(t *testing.T) {
	if logger(nil) == nil {
		t.Fatal("logger(nil) returned nil")
	}
	logger(nil).Info("this must not panic")

	custom := testLogger()
	if logger(custom) != custom {
		t.Fatal("logger replaced the caller's logger")
	}
}
