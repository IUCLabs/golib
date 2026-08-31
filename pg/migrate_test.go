package pg

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testFS is a three-file migration set in deliberately unsorted map order.
func testFS() fstest.MapFS {
	return fstest.MapFS{
		"0002_second.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS b ();")},
		"0001_first.sql":  {Data: []byte("CREATE TABLE IF NOT EXISTS a ();")},
		"0003_third.sql":  {Data: []byte("CREATE TABLE IF NOT EXISTS c ();")},
		"README.md":       {Data: []byte("not a migration")},
	}
}

var testNames = []string{"0001_first.sql", "0002_second.sql", "0003_third.sql"}

// fakeDB stands in for Postgres: the advisory lock is a mutex, the version
// table an in-memory set, and a migration is "run" by recording its version.
type fakeDB struct {
	lock sync.Mutex // the advisory lock: one holder at a time, cluster-wide

	mu           sync.Mutex
	held         bool
	tableCreated bool
	applied      []string
	applyCalls   []string
	bodies       []string
	failOn       string
	unlockErr    error
	discards     int
	violations   []string
}

type fakeMigrator struct{ db *fakeDB }

func (f *fakeMigrator) Lock(ctx context.Context) error {
	f.db.lock.Lock()
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.db.held = true
	return nil
}

func (f *fakeMigrator) Unlock(ctx context.Context) error {
	f.db.mu.Lock()
	f.db.held = false
	err := f.db.unlockErr
	f.db.mu.Unlock()
	f.db.lock.Unlock()
	return err
}

func (f *fakeMigrator) Discard(ctx context.Context) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.db.discards++
}

// requireHeld records any access made without the advisory lock; every schema
// read or write must happen inside the locked section.
func (f *fakeMigrator) requireHeld(op string) {
	if !f.db.held {
		f.db.violations = append(f.db.violations, op)
	}
}

func (f *fakeMigrator) EnsureVersionTable(ctx context.Context) error {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.requireHeld("EnsureVersionTable")
	f.db.tableCreated = true
	return nil
}

func (f *fakeMigrator) AppliedVersions(ctx context.Context) (map[string]bool, error) {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.requireHeld("AppliedVersions")
	if !f.db.tableCreated {
		return nil, errors.New("schema_migrations does not exist")
	}
	out := map[string]bool{}
	for _, v := range f.db.applied {
		out[v] = true
	}
	return out, nil
}

func (f *fakeMigrator) Apply(ctx context.Context, version, sql string) error {
	f.db.mu.Lock()
	defer f.db.mu.Unlock()
	f.requireHeld("Apply")
	f.db.applyCalls = append(f.db.applyCalls, version)
	f.db.bodies = append(f.db.bodies, sql)
	if sql == "" {
		return errors.New("empty migration body")
	}
	// A failing migration rolls back, so neither the SQL nor the version row lands.
	if version == f.db.failOn {
		return errors.New("boom")
	}
	f.db.applied = append(f.db.applied, version)
	return nil
}

func (f *fakeDB) snapshot() (applied, calls, violations []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applied...),
		append([]string(nil), f.applyCalls...),
		append([]string(nil), f.violations...)
}

func (f *fakeDB) discardCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discards
}

// arrproxy pinned its advisory lock key as a literal derived from
// FNV-1a/64("arrproxy-migrations"). LockKey must reproduce it exactly, or
// adopting this package would silently stop excluding the old deployment.
func TestLockKey_MatchesTheEstatesPinnedKey(t *testing.T) {
	const arrproxyKey int64 = 7816645656172167846
	if got := LockKey("arrproxy-migrations"); got != arrproxyKey {
		t.Fatalf("LockKey(%q) = %d, want %d", "arrproxy-migrations", got, arrproxyKey)
	}
}

func TestLockKey_IsFNV1a64(t *testing.T) {
	for _, name := range []string{"", "encapi-migrations", "a much longer lock name"} {
		h := fnv.New64a()
		if _, err := h.Write([]byte(name)); err != nil {
			t.Fatalf("hash: %v", err)
		}
		if got, want := LockKey(name), int64(h.Sum64()); got != want {
			t.Errorf("LockKey(%q) = %d, want %d", name, got, want)
		}
	}
}

// Distinct names must not collide, or two services sharing a cluster would
// serialise against each other by accident.
func TestLockKey_DistinctNamesDistinctKeys(t *testing.T) {
	if LockKey("encapi-migrations") == LockKey("artifactapi-migrations") {
		t.Fatal("two different lock names produced the same key")
	}
}

func TestMigrationNames_SortedSQLOnly(t *testing.T) {
	got, err := migrationNames(testFS())
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	if !reflect.DeepEqual(got, testNames) {
		t.Fatalf("got %v, want %v", got, testNames)
	}
}

func TestMigrationNames_IgnoresDirectories(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_first.sql":     {Data: []byte("SELECT 1;")},
		"sub.sql/keep.sql":   {Data: []byte("SELECT 1;")},
		"0002_second.sql.gz": {Data: []byte("SELECT 1;")},
	}
	got, err := migrationNames(fsys)
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	if want := []string{"0001_first.sql"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMigrationNames_EmptySetIsAnError(t *testing.T) {
	if _, err := migrationNames(fstest.MapFS{}); err == nil {
		t.Fatal("expected an error for an empty migration set")
	}
}

func TestMigrationNames_UnreadableFSIsAnError(t *testing.T) {
	if _, err := migrationNames(brokenFS{}); err == nil {
		t.Fatal("expected an error when the migration directory cannot be read")
	}
}

func TestRunMigrations_FreshDatabaseAppliesAllInOrder(t *testing.T) {
	db := &fakeDB{}
	if err := runMigrations(context.Background(), &fakeMigrator{db: db}, testFS(), testLogger()); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	applied, _, violations := db.snapshot()
	if !reflect.DeepEqual(applied, testNames) {
		t.Fatalf("applied %v, want %v", applied, testNames)
	}
	if !db.tableCreated {
		t.Error("schema_migrations was not created")
	}
	if len(violations) != 0 {
		t.Errorf("schema accessed without the advisory lock: %v", violations)
	}
}

// The runner must hand Apply the file's bytes, not just its name.
func TestRunMigrations_PassesFileBodies(t *testing.T) {
	db := &fakeDB{}
	fsys := testFS()
	if err := runMigrations(context.Background(), &fakeMigrator{db: db}, fsys, testLogger()); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for i, name := range testNames {
		if want := string(fsys[name].Data); db.bodies[i] != want {
			t.Errorf("body %d = %q, want %q", i, db.bodies[i], want)
		}
	}
}

func TestRunMigrations_SecondRunIsANoOp(t *testing.T) {
	db := &fakeDB{}
	ctx := context.Background()
	for i := range 2 {
		if err := runMigrations(ctx, &fakeMigrator{db: db}, testFS(), testLogger()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	applied, calls, _ := db.snapshot()
	if !reflect.DeepEqual(applied, testNames) {
		t.Fatalf("applied %v, want %v", applied, testNames)
	}
	if len(calls) != len(testNames) {
		t.Fatalf("Apply called %d times across two runs, want %d", len(calls), len(testNames))
	}
}

// A file missing from schema_migrations is re-applied even when the rest of the
// set is recorded: that is how a schema applied out of band is adopted.
func TestRunMigrations_AppliesOnlyTheMissingVersion(t *testing.T) {
	db := &fakeDB{tableCreated: true, applied: []string{testNames[0], testNames[2]}}
	if err := runMigrations(context.Background(), &fakeMigrator{db: db}, testFS(), testLogger()); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	_, calls, _ := db.snapshot()
	if want := testNames[1:2]; !reflect.DeepEqual(calls, want) {
		t.Fatalf("Apply calls %v, want %v", calls, want)
	}
}

// Replicas starting together queue on the advisory lock: the first applies the
// set, the rest find it already recorded and do nothing.
func TestRunMigrations_ConcurrentStartersSerialize(t *testing.T) {
	db := &fakeDB{}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = runMigrations(ctx, &fakeMigrator{db: db}, testFS(), testLogger())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("starter %d: %v", i, err)
		}
	}
	applied, calls, violations := db.snapshot()
	if !reflect.DeepEqual(applied, testNames) {
		t.Fatalf("applied %v, want %v", applied, testNames)
	}
	if len(calls) != len(testNames) {
		t.Fatalf("Apply called %d times, want %d (one starter should have applied)", len(calls), len(testNames))
	}
	if len(violations) != 0 {
		t.Errorf("schema accessed without the advisory lock: %v", violations)
	}
}

// A migration that fails mid-set aborts the run, leaves earlier versions
// recorded, and never reaches later ones.
func TestRunMigrations_FailureStopsAndKeepsEarlierVersions(t *testing.T) {
	db := &fakeDB{failOn: testNames[1]}
	err := runMigrations(context.Background(), &fakeMigrator{db: db}, testFS(), testLogger())
	if err == nil {
		t.Fatal("expected the failing migration to abort the run")
	}
	if !strings.Contains(err.Error(), testNames[1]) {
		t.Errorf("error %q does not name the failing migration", err)
	}
	applied, calls, _ := db.snapshot()
	if !reflect.DeepEqual(applied, testNames[:1]) {
		t.Fatalf("applied %v, want %v", applied, testNames[:1])
	}
	if !reflect.DeepEqual(calls, testNames[:2]) {
		t.Fatalf("Apply calls %v, want %v (later migrations must not run)", calls, testNames[:2])
	}
	// The lock must be released even on failure, or every later pod deadlocks.
	if !db.lock.TryLock() {
		t.Fatal("advisory lock still held after a failed run")
	}
	db.lock.Unlock()
}

// A run that cannot take the lock must not touch the schema.
func TestRunMigrations_LockFailureAborts(t *testing.T) {
	db := &fakeDB{}
	m := &failingLockMigrator{fakeMigrator{db: db}}
	if err := runMigrations(context.Background(), m, testFS(), testLogger()); err == nil {
		t.Fatal("expected a lock failure to abort the run")
	}
	applied, calls, _ := db.snapshot()
	if len(applied) != 0 || len(calls) != 0 {
		t.Fatalf("migrations ran without the lock: applied %v, calls %v", applied, calls)
	}
	if db.tableCreated {
		t.Error("schema_migrations was created without the lock")
	}
}

// A run that cannot see its bookkeeping table fails instead of re-applying blind.
func TestRunMigrations_ReadAppliedFailureAborts(t *testing.T) {
	// tableCreated stays false because EnsureVersionTable is a no-op here, so
	// the fake's AppliedVersions reports the table as missing.
	db := &fakeDB{}
	m := &noTableMigrator{fakeMigrator{db: db}}
	err := runMigrations(context.Background(), m, testFS(), testLogger())
	if err == nil {
		t.Fatal("expected the run to fail when applied versions cannot be read")
	}
	if !strings.Contains(err.Error(), "read applied migrations") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
	_, calls, _ := db.snapshot()
	if len(calls) != 0 {
		t.Fatalf("migrations applied without reading the version table: %v", calls)
	}
}

func TestRunMigrations_EnsureVersionTableFailureAborts(t *testing.T) {
	db := &fakeDB{}
	m := &failingEnsureMigrator{fakeMigrator{db: db}}
	err := runMigrations(context.Background(), m, testFS(), testLogger())
	if err == nil {
		t.Fatal("expected the run to fail when schema_migrations cannot be created")
	}
	if !strings.Contains(err.Error(), "ensure schema_migrations") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
	// Even this early failure must release the lock.
	if !db.lock.TryLock() {
		t.Fatal("advisory lock still held")
	}
	db.lock.Unlock()
}

func TestRunMigrations_UnreadableMigrationAborts(t *testing.T) {
	db := &fakeDB{}
	// A directory entry that ReadDir reports as a file but ReadFile cannot open.
	err := runMigrations(context.Background(), &fakeMigrator{db: db}, missingFileFS{}, testLogger())
	if err == nil {
		t.Fatal("expected an unreadable migration to abort the run")
	}
	if !strings.Contains(err.Error(), "read migration") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

// An unlock that does not land leaves a session that may still hold the lock, so
// the connection is discarded rather than returned to the pool. The migrations
// themselves already succeeded, so the run still reports success.
func TestRunMigrations_UnlockFailureDiscardsConnection(t *testing.T) {
	db := &fakeDB{unlockErr: errors.New("connection reset")}
	if err := runMigrations(context.Background(), &fakeMigrator{db: db}, testFS(), testLogger()); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if got := db.discardCount(); got != 1 {
		t.Fatalf("Discard called %d times after a failed unlock, want 1", got)
	}
	applied, _, _ := db.snapshot()
	if !reflect.DeepEqual(applied, testNames) {
		t.Fatalf("applied %v, want %v", applied, testNames)
	}
}

// A clean unlock keeps the connection: discarding every migration connection
// would churn the pool on every start.
func TestRunMigrations_CleanUnlockKeepsConnection(t *testing.T) {
	db := &fakeDB{}
	if err := runMigrations(context.Background(), &fakeMigrator{db: db}, testFS(), testLogger()); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if got := db.discardCount(); got != 0 {
		t.Fatalf("Discard called %d times after a clean unlock, want 0", got)
	}
}

type failingLockMigrator struct{ fakeMigrator }

func (f *failingLockMigrator) Lock(ctx context.Context) error { return errors.New("lock unavailable") }

type failingEnsureMigrator struct{ fakeMigrator }

func (f *failingEnsureMigrator) EnsureVersionTable(ctx context.Context) error {
	return errors.New("permission denied")
}

type noTableMigrator struct{ fakeMigrator }

// EnsureVersionTable silently does nothing, so AppliedVersions then fails.
func (f *noTableMigrator) EnsureVersionTable(ctx context.Context) error { return nil }
