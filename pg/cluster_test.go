package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A second unreachable DSN, distinct from deadDSN, so a test can tell the two
// pools of a Cluster apart by pointer.
const deadReplicaDSN = "postgres://u:p@127.0.0.1:2/d?sslmode=disable&connect_timeout=2"

// fakePinger answers probes from a scripted sequence of results, so recovery
// can be driven without a database.
type fakePinger struct {
	results []error
	calls   int
}

func (p *fakePinger) Ping(context.Context) error {
	p.calls++
	if len(p.results) == 0 {
		return nil
	}
	i := min(p.calls-1, len(p.results)-1)
	return p.results[i]
}

// fakeClock is a manually advanced clock for the backoff window.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// lazyPool opens a pool without connecting: pgxpool is lazy, so this needs no
// server and lets a test build a Cluster with two distinguishable pools.
func lazyPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New(%q): %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testCluster builds a Cluster with a replica and a circuit under a fake clock.
func testCluster(t *testing.T, probe pinger) (*Cluster, *fakeClock) {
	t.Helper()
	c := &Cluster{
		primary: lazyPool(t, deadDSN),
		replica: lazyPool(t, deadReplicaDSN),
	}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.health = newReplicaCircuit(probe, testLogger(), time.Second, 4*time.Second)
	c.health.now = clock.now
	return c, clock
}

// The routing table: what Read and Write return in each replica state.
func TestCluster_Routing(t *testing.T) {
	single := &Cluster{primary: lazyPool(t, deadDSN)}
	if single.Read() != single.primary {
		t.Error("with no replica, Read must return the primary")
	}
	if single.Write() != single.primary || single.Primary() != single.primary {
		t.Error("Write and Primary must return the primary")
	}

	c, clock := testCluster(t, &fakePinger{})
	if c.Read() != c.replica {
		t.Fatal("a healthy replica must serve reads")
	}
	if c.Write() != c.primary {
		t.Fatal("writes must always go to the primary")
	}
	if c.Primary() != c.primary {
		t.Fatal("Primary must return the primary even with a healthy replica")
	}

	c.ReportReplicaError(errors.New("replica exploded"))
	if c.Read() != c.primary {
		t.Fatal("an unhealthy replica must not serve reads")
	}

	// Inside the backoff window the decision stands without a probe.
	clock.advance(500 * time.Millisecond)
	if c.Read() != c.primary {
		t.Fatal("reads must stay on the primary inside the backoff window")
	}

	// Once the window expires the next read probes, and a healthy probe
	// promotes the replica again.
	clock.advance(time.Second)
	if c.Read() != c.replica {
		t.Fatal("a recovered replica must serve reads again")
	}
}

// A nil error and a Cluster without a replica must both be no-ops rather than a
// nil-pointer dereference on the circuit.
func TestCluster_ReportReplicaErrorIgnoresNoOps(t *testing.T) {
	single := &Cluster{primary: lazyPool(t, deadDSN)}
	single.ReportReplicaError(errors.New("no replica to blame"))

	c, _ := testCluster(t, &fakePinger{})
	c.ReportReplicaError(nil)
	if c.Read() != c.replica {
		t.Fatal("a nil error must not trip the circuit")
	}
}

func TestReplicaCircuit_BackoffDoublesToTheCeiling(t *testing.T) {
	probe := &fakePinger{results: []error{errors.New("down")}}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newReplicaCircuit(probe, testLogger(), time.Second, 4*time.Second)
	c.now = clock.now

	c.trip(errors.New("down"))
	// 1s, then 2s, then 4s, then held at the 4s ceiling.
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if c.backoff != want {
			t.Fatalf("backoff = %s, want %s", c.backoff, want)
		}
		// Just short of the window: no probe, still tripped.
		before := probe.calls
		clock.advance(want - time.Millisecond)
		if c.use() {
			t.Fatal("the circuit reopened before its window expired")
		}
		if probe.calls != before {
			t.Fatal("the circuit probed inside its window")
		}
		clock.advance(time.Millisecond)
		if c.use() {
			t.Fatal("a failing probe must leave the circuit closed")
		}
		if probe.calls != before+1 {
			t.Fatalf("probe calls = %d, want %d", probe.calls, before+1)
		}
	}
}

// Reported errors while already tripped must not push the retry out: a busy
// service reporting on every read would otherwise never probe again.
func TestReplicaCircuit_RepeatedTripsKeepTheWindow(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newReplicaCircuit(&fakePinger{}, testLogger(), time.Second, 4*time.Second)
	c.now = clock.now

	c.trip(errors.New("first"))
	retryAt := c.retryAt
	clock.advance(900 * time.Millisecond)
	c.trip(errors.New("second"))
	if !c.retryAt.Equal(retryAt) {
		t.Fatalf("retryAt moved from %s to %s on a repeated trip", retryAt, c.retryAt)
	}
}

// After a recovery the backoff restarts at the minimum rather than resuming
// where the previous outage left off.
func TestReplicaCircuit_RecoveryResetsTheBackoff(t *testing.T) {
	probe := &fakePinger{results: []error{errors.New("down"), nil}}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newReplicaCircuit(probe, testLogger(), time.Second, 4*time.Second)
	c.now = clock.now

	c.trip(errors.New("down"))
	clock.advance(time.Second)
	if c.use() {
		t.Fatal("the first probe fails, so the circuit stays closed")
	}
	clock.advance(2 * time.Second)
	if !c.use() {
		t.Fatal("the second probe succeeds, so the circuit must reopen")
	}
	if c.backoff != 0 {
		t.Fatalf("backoff = %s after recovery, want 0", c.backoff)
	}

	c.trip(errors.New("down again"))
	if c.backoff != time.Second {
		t.Fatalf("backoff = %s on the next outage, want the 1s minimum", c.backoff)
	}
}

func TestNewReplicaCircuit_Defaults(t *testing.T) {
	c := newReplicaCircuit(&fakePinger{}, testLogger(), 0, 0)
	if c.min != defaultReplicaRetryMin || c.max != defaultReplicaRetryMax {
		t.Fatalf("defaults = %s/%s, want %s/%s", c.min, c.max, defaultReplicaRetryMin, defaultReplicaRetryMax)
	}
	// A minimum above the default ceiling must not produce max < min.
	c = newReplicaCircuit(&fakePinger{}, testLogger(), time.Hour, 0)
	if c.max < c.min {
		t.Fatalf("max %s is below min %s", c.max, c.min)
	}
}

func TestNewCluster_SinglePoolModes(t *testing.T) {
	tests := []struct {
		name string
		cfg  ClusterConfig
	}{
		{"no replica configured", ClusterConfig{PrimaryDSN: deadDSN}},
		{"replica DSN equal to the primary", ClusterConfig{PrimaryDSN: deadDSN, ReplicaDSN: deadDSN}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := newCluster(shortCtx(t), lazyPool(t, deadDSN), tc.cfg)
			if err != nil {
				t.Fatalf("newCluster: %v", err)
			}
			if c.replica != nil {
				t.Fatal("expected single-pool mode")
			}
			if c.Read() != c.Write() {
				t.Fatal("in single-pool mode Read and Write must be the same pool")
			}
		})
	}
}

// A replica that will not answer must degrade to primary reads, not fail
// startup: the service can still serve everything, just without the split.
func TestNewCluster_UnreachableReplicaStartsDegraded(t *testing.T) {
	c, err := newCluster(shortCtx(t), lazyPool(t, deadDSN), ClusterConfig{
		PrimaryDSN: deadDSN,
		ReplicaDSN: deadReplicaDSN,
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("newCluster: %v", err)
	}
	t.Cleanup(func() { c.replica.Close() })
	if c.replica == nil {
		t.Fatal("the replica pool must still be opened")
	}
	if !c.health.tripped {
		t.Fatal("a replica that failed its startup ping must start tripped")
	}
	if c.Read() != c.primary {
		t.Fatal("reads must start on the primary while the replica is down")
	}
}

func TestNewCluster_RejectsAnUnparseableReplicaDSN(t *testing.T) {
	_, err := newCluster(shortCtx(t), lazyPool(t, deadDSN), ClusterConfig{
		PrimaryDSN: deadDSN,
		ReplicaDSN: "://not a dsn",
	})
	if err == nil {
		t.Fatal("expected an error for an unparseable replica DSN")
	}
	if !strings.Contains(err.Error(), "connect postgres replica") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

func TestNewCluster_PropagatesPrimaryFailure(t *testing.T) {
	c, err := NewCluster(shortCtx(t), ClusterConfig{PrimaryDSN: deadDSN, ReplicaDSN: deadReplicaDSN})
	if err == nil {
		c.Close()
		t.Fatal("expected NewCluster to fail against an unreachable primary")
	}
	if !strings.Contains(err.Error(), "ping postgres") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

// Migrations must reach the primary even when the replica is the pool Read
// would hand out. A closed replica pool fails Acquire with a distinctive error,
// so routing the run to it would be visible here.
func TestCluster_MigrateTargetsThePrimary(t *testing.T) {
	c, _ := testCluster(t, &fakePinger{})
	if c.Read() != c.replica {
		t.Fatal("the replica must be healthy for this test to mean anything")
	}
	c.replica.Close()

	err := c.Migrate(shortCtx(t), testFS(), MigrateOptions{LockName: testLockName})
	if err == nil {
		t.Fatal("expected the migration to fail against an unreachable primary")
	}
	if strings.Contains(err.Error(), "closed pool") {
		t.Fatalf("the migration ran against the replica: %v", err)
	}
	if !strings.Contains(err.Error(), "acquire migration connection") {
		t.Fatalf("error %q does not identify the failing step", err)
	}
}

func TestCluster_MigrateRequiresALockName(t *testing.T) {
	c := &Cluster{primary: lazyPool(t, deadDSN)}
	if err := c.Migrate(shortCtx(t), fstest.MapFS{}, MigrateOptions{}); err == nil {
		t.Fatal("expected Migrate to reject an empty LockName")
	}
}

func TestCluster_CloseClosesBothPools(t *testing.T) {
	c := &Cluster{
		primary: lazyPool(t, deadDSN),
		replica: lazyPool(t, deadReplicaDSN),
	}
	c.health = newReplicaCircuit(c.replica, testLogger(), 0, 0)
	c.Close()

	// Acquiring from a closed pool fails immediately; a live one would dial.
	for name, pool := range map[string]*pgxpool.Pool{"primary": c.primary, "replica": c.replica} {
		if _, err := pool.Acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "closed pool") {
			t.Errorf("%s pool was not closed: %v", name, err)
		}
	}
	// Close is registered again by lazyPool's cleanup; pgxpool tolerates it.
}
