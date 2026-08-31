package pg

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const testLockName = "golib-test-migrations"

func newTestMigrator(s *fakeSession) (*pgMigrator, *int) {
	discards := 0
	m := &pgMigrator{
		conn:    s,
		key:     LockKey(testLockName),
		discard: func(context.Context) { discards++ },
	}
	return m, &discards
}

// The lock and unlock must name the same key, and it must be the derived one:
// a mismatch here is a deadlock or a lock nobody else respects.
func TestPgMigrator_LockUnlockUseTheDerivedKey(t *testing.T) {
	s := &fakeSession{}
	m, _ := newTestMigrator(s)
	ctx := context.Background()

	if err := m.Lock(ctx); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := m.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	want := []string{"SELECT pg_advisory_lock($1)", "SELECT pg_advisory_unlock($1)"}
	if got := s.execSQL(); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements %v, want %v", got, want)
	}
	key := LockKey(testLockName)
	for _, c := range s.execs {
		if len(c.args) != 1 || c.args[0] != key {
			t.Fatalf("%q got args %v, want [%d]", c.sql, c.args, key)
		}
	}
}

func TestPgMigrator_LockAndUnlockPropagateErrors(t *testing.T) {
	lockErr := errors.New("lock failed")
	unlockErr := errors.New("unlock failed")
	s := &fakeSession{execErrs: map[string]error{
		"SELECT pg_advisory_lock($1)":   lockErr,
		"SELECT pg_advisory_unlock($1)": unlockErr,
	}}
	m, _ := newTestMigrator(s)
	if err := m.Lock(context.Background()); !errors.Is(err, lockErr) {
		t.Errorf("Lock error = %v, want %v", err, lockErr)
	}
	if err := m.Unlock(context.Background()); !errors.Is(err, unlockErr) {
		t.Errorf("Unlock error = %v, want %v", err, unlockErr)
	}
}

func TestPgMigrator_DiscardClosesTheSession(t *testing.T) {
	m, discards := newTestMigrator(&fakeSession{})
	m.Discard(context.Background())
	if *discards != 1 {
		t.Fatalf("discard called %d times, want 1", *discards)
	}
}

func TestPgMigrator_EnsureVersionTableRunsTheDDL(t *testing.T) {
	s := &fakeSession{}
	m, _ := newTestMigrator(s)
	if err := m.EnsureVersionTable(context.Background()); err != nil {
		t.Fatalf("EnsureVersionTable: %v", err)
	}
	if got := s.execSQL(); !reflect.DeepEqual(got, []string{schemaMigrationsDDL}) {
		t.Fatalf("statements %v, want the schema_migrations DDL", got)
	}
	// Re-running the DDL must be harmless, so it has to be IF NOT EXISTS.
	if !strings.Contains(schemaMigrationsDDL, "IF NOT EXISTS") {
		t.Error("schema_migrations DDL is not IF NOT EXISTS-guarded")
	}
}

func TestPgMigrator_EnsureVersionTablePropagatesErrors(t *testing.T) {
	want := errors.New("permission denied")
	s := &fakeSession{execErrs: map[string]error{schemaMigrationsDDL: want}}
	m, _ := newTestMigrator(s)
	if err := m.EnsureVersionTable(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestPgMigrator_AppliedVersionsReadsAndClosesRows(t *testing.T) {
	rows := &fakeRows{values: []string{"0001_first.sql", "0002_second.sql"}}
	s := &fakeSession{rows: rows}
	m, _ := newTestMigrator(s)

	got, err := m.AppliedVersions(context.Background())
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	want := map[string]bool{"0001_first.sql": true, "0002_second.sql": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("versions %v, want %v", got, want)
	}
	if !rows.closed {
		t.Error("rows were not closed")
	}
	if wantQ := []string{"SELECT version FROM schema_migrations"}; !reflect.DeepEqual(s.queries, wantQ) {
		t.Fatalf("queries %v, want %v", s.queries, wantQ)
	}
}

func TestPgMigrator_AppliedVersionsEmptyTable(t *testing.T) {
	s := &fakeSession{rows: &fakeRows{}}
	m, _ := newTestMigrator(s)
	got, err := m.AppliedVersions(context.Background())
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("versions %v, want an empty set", got)
	}
}

func TestPgMigrator_AppliedVersionsErrors(t *testing.T) {
	queryErr := errors.New("relation does not exist")
	scanErr := errors.New("bad column type")
	rowsErr := errors.New("connection reset mid-read")

	tests := []struct {
		name string
		sess *fakeSession
		want error
	}{
		{"query fails", &fakeSession{queryErr: queryErr}, queryErr},
		{"scan fails", &fakeSession{rows: &fakeRows{values: []string{"x"}, scanErr: scanErr}}, scanErr},
		// A read that dies part-way must not be reported as a complete set, or
		// already-applied migrations would be re-run.
		{"rows.Err after iteration", &fakeSession{rows: &fakeRows{values: []string{"x"}, err: rowsErr}}, rowsErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMigrator(tc.sess)
			got, err := m.AppliedVersions(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Fatalf("versions = %v, want nil on error", got)
			}
		})
	}
}

// One transaction carries both the migration body and its tracking row, so a
// crash can never leave the schema changed but unrecorded.
func TestPgMigrator_ApplyRunsBodyAndVersionRowInOneTx(t *testing.T) {
	const body = "CREATE TABLE IF NOT EXISTS a ();"
	tx := &fakeTx{}
	s := &fakeSession{tx: tx}
	m, _ := newTestMigrator(s)

	if err := m.Apply(context.Background(), "0001_first.sql", body); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{body, "INSERT INTO schema_migrations (version) VALUES ($1)"}
	if got := tx.execSQL(); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements %v, want %v", got, want)
	}
	// The body must be sent with no arguments so pgx uses the simple protocol
	// and a multi-statement file runs.
	if len(tx.execs[0].args) != 0 {
		t.Errorf("migration body sent with args %v, want none", tx.execs[0].args)
	}
	if got := tx.execs[1].args; len(got) != 1 || got[0] != "0001_first.sql" {
		t.Errorf("version row args %v, want [0001_first.sql]", got)
	}
	if !tx.committed {
		t.Error("transaction was not committed")
	}
	if tx.rolled != 1 {
		t.Errorf("Rollback called %d times, want 1 (the no-op deferred rollback)", tx.rolled)
	}
}

func TestPgMigrator_ApplyRollsBackOnFailure(t *testing.T) {
	const body = "CREATE TABLE oops ();"
	bodyErr := errors.New("syntax error")
	insertErr := errors.New("duplicate key")
	const insert = "INSERT INTO schema_migrations (version) VALUES ($1)"

	tests := []struct {
		name      string
		execErrs  map[string]error
		commitErr error
		want      error
		wantExecs int
	}{
		{"body fails", map[string]error{body: bodyErr}, nil, bodyErr, 1},
		{"version row fails", map[string]error{insert: insertErr}, nil, insertErr, 2},
		{"commit fails", nil, errors.New("commit failed"), nil, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTx{execErrs: tc.execErrs, commitErr: tc.commitErr}
			s := &fakeSession{tx: tx}
			m, _ := newTestMigrator(s)

			err := m.Apply(context.Background(), "0001_first.sql", body)
			if err == nil {
				t.Fatal("expected Apply to fail")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(tx.execs) != tc.wantExecs {
				t.Errorf("%d statements ran, want %d", len(tx.execs), tc.wantExecs)
			}
			if tx.committed {
				t.Error("transaction was committed despite the failure")
			}
			if tx.rolled == 0 {
				t.Error("transaction was not rolled back")
			}
		})
	}
}

func TestPgMigrator_ApplyPropagatesBeginFailure(t *testing.T) {
	want := errors.New("cannot start transaction")
	m, _ := newTestMigrator(&fakeSession{beginErr: want})
	if err := m.Apply(context.Background(), "0001_first.sql", "SELECT 1;"); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// migrateSession is the whole runner over a real pgMigrator: lock, DDL, read,
// apply each file, unlock — in that order, on one session.
func TestMigrateSession_FullRunOnOneSession(t *testing.T) {
	tx := &fakeTx{}
	s := &fakeSession{rows: &fakeRows{}, tx: tx}
	discards := 0
	err := migrateSession(context.Background(), s,
		func(context.Context) { discards++ },
		testFS(),
		MigrateOptions{LockName: testLockName})
	if err != nil {
		t.Fatalf("migrateSession: %v", err)
	}

	want := []string{
		"SELECT pg_advisory_lock($1)",
		schemaMigrationsDDL,
		"SELECT pg_advisory_unlock($1)",
	}
	if got := s.execSQL(); !reflect.DeepEqual(got, want) {
		t.Fatalf("session statements %v, want %v", got, want)
	}
	// Every file's body plus its version row, all through the one fake tx.
	if got, wantN := len(tx.execs), 2*len(testNames); got != wantN {
		t.Fatalf("%d statements in transactions, want %d", got, wantN)
	}
	if discards != 0 {
		t.Errorf("connection discarded %d times after a clean run, want 0", discards)
	}
}

func TestMigrateSession_WrapsRunnerErrors(t *testing.T) {
	s := &fakeSession{execErrs: map[string]error{"SELECT pg_advisory_lock($1)": errors.New("no lock")}}
	err := migrateSession(context.Background(), s, func(context.Context) {}, testFS(),
		MigrateOptions{LockName: testLockName})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "migrate schema") {
		t.Fatalf("error %q is not wrapped with the migration context", err)
	}
}

// An unlock that fails must reach the discard func through the real migrator,
// not just the fake one the runner tests use.
func TestMigrateSession_UnlockFailureDiscards(t *testing.T) {
	s := &fakeSession{
		rows:     &fakeRows{},
		tx:       &fakeTx{},
		execErrs: map[string]error{"SELECT pg_advisory_unlock($1)": errors.New("gone")},
	}
	discards := 0
	err := migrateSession(context.Background(), s, func(context.Context) { discards++ },
		testFS(), MigrateOptions{LockName: testLockName})
	if err != nil {
		t.Fatalf("migrateSession: %v", err)
	}
	if discards != 1 {
		t.Fatalf("connection discarded %d times, want 1", discards)
	}
}
