package pg

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New opens a pgxpool against dsn and verifies it can reach the server before
// returning. pgxpool connects lazily, so without the ping a bad address only
// surfaces on the first query, long after startup has reported success.
//
// The caller owns the pool and must Close it. log may be nil.
func New(ctx context.Context, dsn string, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	logger(log).Debug("postgres pool ready", "host", pool.Config().ConnConfig.Host)
	return pool, nil
}

// NewMigrated opens a pool and brings the schema up to date before returning
// it, so a service cannot start serving against a half-migrated database. A
// migration failure closes the pool and returns the error; callers treat it as
// fatal.
func NewMigrated(ctx context.Context, dsn string, fsys fs.FS, opts MigrateOptions) (*pgxpool.Pool, error) {
	pool, err := New(ctx, dsn, opts.Logger)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, pool, fsys, opts); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
