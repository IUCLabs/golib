// Package pgtest starts a throwaway Postgres container for integration-style
// tests. Import it only from _test.go files so testcontainers never reaches a
// production binary.
//
// The estate's CI runs on Kubernetes with no Docker socket, so tests that need
// a container must skip themselves under -short. SkipIfShort does exactly that,
// and StartPostgres reports a plain error when no container runtime is
// reachable, leaving it to the caller whether that is a skip or a failure.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the Postgres the estate targets; every service runs the same major.
const Image = "postgres:17-alpine"

const (
	database = "pgtest"
	username = "pgtest"
	password = "pgtest123"
)

// startTimeout bounds the container's readiness wait.
const startTimeout = 60 * time.Second

func init() {
	// The Ryuk reaper container cannot start in every environment (rootless
	// podman, restricted CI). Callers get an explicit terminate func instead,
	// so disable Ryuk unless the environment has deliberately enabled it.
	if _, ok := os.LookupEnv("TESTCONTAINERS_RYUK_DISABLED"); !ok {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// SkipIfShort skips the test under -short. Container-backed tests call it
// first, which is what keeps `go test -short ./...` green with no Docker.
func SkipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
}

// StartPostgres launches a Postgres container and returns its DSN plus a
// terminate func the caller must run, even on failure paths.
func StartPostgres(ctx context.Context) (dsn string, terminate func(), err error) {
	c, err := tcpostgres.Run(ctx,
		Image,
		tcpostgres.WithDatabase(database),
		tcpostgres.WithUsername(username),
		tcpostgres.WithPassword(password),
		testcontainers.WithWaitStrategy(
			// Postgres opens the port, runs its init scripts, then restarts, so
			// wait for the readiness log twice to avoid connection resets.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startTimeout),
		),
	)
	if err != nil {
		return "", nil, fmt.Errorf("start postgres container: %w", err)
	}
	terminate = func() { _ = c.Terminate(ctx) }

	host, err := c.Host(ctx)
	if err != nil {
		terminate()
		return "", nil, fmt.Errorf("container host: %w", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		terminate()
		return "", nil, fmt.Errorf("container port: %w", err)
	}
	dsn = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		username, password, host, port.Port(), database)
	return dsn, terminate, nil
}

// MustStartPostgres is StartPostgres for a TestMain-less test: it skips under
// -short, fails the test if the container cannot start, and registers cleanup.
func MustStartPostgres(ctx context.Context, t *testing.T) string {
	t.Helper()
	SkipIfShort(t)
	dsn, terminate, err := StartPostgres(ctx)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(terminate)
	return dsn
}
