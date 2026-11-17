// Package pg holds shared Postgres plumbing: environment-driven
// DSN construction, pgxpool construction, read/write splitting across a primary
// and a read replica, and the migration runner every service uses to bring its
// own schema up to date at startup.
package pg

import (
	"io"
	"log/slog"
)

// logger returns log, or a logger that discards everything when log is nil, so
// callers may leave the option unset.
func logger(log *slog.Logger) *slog.Logger {
	if log != nil {
		return log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
