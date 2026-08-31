package pg

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Replica health defaults. The retry window is deliberately coarse: a replica
// that just fell over is not coming back within a request, and probing it more
// often only moves the failure onto the read path.
const (
	defaultReplicaRetryMin = 5 * time.Second
	defaultReplicaRetryMax = 2 * time.Minute
	replicaProbeTimeout    = 5 * time.Second
)

// ClusterConfig configures a Cluster.
type ClusterConfig struct {
	// PrimaryDSN is the read-write endpoint. Required.
	PrimaryDSN string

	// ReplicaDSN is the read-only endpoint. Empty puts the Cluster in
	// single-pool mode, where Read returns the primary. A ReplicaDSN equal to
	// PrimaryDSN does the same rather than opening a second pool to the same
	// place.
	ReplicaDSN string

	// Logger receives replica health transitions. Nil discards them.
	Logger *slog.Logger

	// ReplicaRetryMin and ReplicaRetryMax bound the backoff window between
	// probes of an unhealthy replica; the window doubles from Min up to Max.
	// Zero means the defaults, 5s and 2m.
	ReplicaRetryMin time.Duration
	ReplicaRetryMax time.Duration
}

// Cluster routes queries across a primary and an optional read replica.
//
// Routing is explicit: the caller picks Read or Write per query. Nothing
// inspects SQL to decide, because statement inspection gets it wrong in both
// directions — a CTE with an INSERT in it reads as a SELECT, and
// SELECT ... FOR UPDATE takes row locks a replica cannot grant.
//
// A Cluster with no replica is a working single-pool Cluster, so a service can
// use it unconditionally and a deployment decides whether reads are split.
type Cluster struct {
	primary *pgxpool.Pool
	// replica is nil in single-pool mode.
	replica *pgxpool.Pool
	health  *replicaCircuit
}

// NewCluster opens the primary and, when configured, the replica.
//
// A primary that cannot be reached is fatal: the service has nowhere to write.
// A replica that cannot be reached is not — the Cluster starts with reads on
// the primary and promotes the replica once it answers a probe, which is the
// same degradation a replica failing later gets.
//
// The caller owns the Cluster and must Close it.
func NewCluster(ctx context.Context, cfg ClusterConfig) (*Cluster, error) {
	primary, err := New(ctx, cfg.PrimaryDSN, cfg.Logger)
	if err != nil {
		return nil, err
	}
	c, err := newCluster(ctx, primary, cfg)
	if err != nil {
		primary.Close()
		return nil, err
	}
	return c, nil
}

// newCluster attaches the replica half to an already-open primary pool.
func newCluster(ctx context.Context, primary *pgxpool.Pool, cfg ClusterConfig) (*Cluster, error) {
	log := logger(cfg.Logger)
	c := &Cluster{primary: primary}
	if cfg.ReplicaDSN == "" || cfg.ReplicaDSN == cfg.PrimaryDSN {
		log.Debug("postgres cluster in single-pool mode, reads go to the primary")
		return c, nil
	}

	replica, err := pgxpool.New(ctx, cfg.ReplicaDSN)
	if err != nil {
		return nil, fmt.Errorf("connect postgres replica: %w", err)
	}
	c.replica = replica
	c.health = newReplicaCircuit(replica, log, cfg.ReplicaRetryMin, cfg.ReplicaRetryMax)
	if err := replica.Ping(ctx); err != nil {
		c.health.trip(err)
	} else {
		log.Debug("postgres replica pool ready", "host", replica.Config().ConnConfig.Host)
	}
	return c, nil
}

// Write returns the primary pool. Every statement that changes data, takes row
// locks, or must be read back immediately goes here.
func (c *Cluster) Write() *pgxpool.Pool { return c.primary }

// Read returns the pool to run a read-only query on: the replica when one is
// configured and healthy, the primary otherwise.
//
// Reads served by a replica see a slightly stale snapshot. Use Primary for a
// read that must observe a write this process just made.
func (c *Cluster) Read() *pgxpool.Pool {
	if c.replica == nil || !c.health.use() {
		return c.primary
	}
	return c.replica
}

// Primary returns the primary pool, whatever the replica's state. It is the
// same pool as Write, named for the read-your-writes case: a read that must not
// be served stale asks for the primary explicitly.
func (c *Cluster) Primary() *pgxpool.Pool { return c.primary }

// ReportReplicaError tells the Cluster a query on the replica failed, so reads
// move to the primary until the replica answers a probe again. Pass the error
// from a query run on the pool Read returned; a nil error, or a Cluster with no
// replica, is a no-op.
//
// It is advisory. A caller that does not report anything still gets correct
// routing, just no reaction to a replica that dies mid-flight.
func (c *Cluster) ReportReplicaError(err error) {
	if err == nil || c.replica == nil {
		return
	}
	c.health.trip(err)
}

// Migrate brings the schema up to date on the primary. Migrations never run
// against a replica: a replica is physically read-only, and a schema change has
// to originate on the primary to reach it at all.
func (c *Cluster) Migrate(ctx context.Context, fsys fs.FS, opts MigrateOptions) error {
	return Migrate(ctx, c.Write(), fsys, opts)
}

// Close closes both pools.
func (c *Cluster) Close() {
	if c.replica != nil {
		c.replica.Close()
	}
	c.primary.Close()
}

// pinger is the slice of *pgxpool.Pool the health circuit drives, so the
// circuit is testable without a database.
type pinger interface {
	Ping(ctx context.Context) error
}

// replicaCircuit tracks whether the replica is worth sending reads to.
//
// While healthy it costs nothing: Read consults a boolean and no probe is
// issued. A reported error or a failed startup ping trips it, and reads go to
// the primary until the backoff window expires, at which point the next Read
// pays for one probe that either promotes the replica or extends the window.
type replicaCircuit struct {
	probe pinger
	log   *slog.Logger
	now   func() time.Time
	min   time.Duration
	max   time.Duration

	mu      sync.Mutex
	tripped bool
	retryAt time.Time
	backoff time.Duration
}

func newReplicaCircuit(probe pinger, log *slog.Logger, retryMin, retryMax time.Duration) *replicaCircuit {
	if retryMin <= 0 {
		retryMin = defaultReplicaRetryMin
	}
	if retryMax < retryMin {
		retryMax = defaultReplicaRetryMax
	}
	if retryMax < retryMin {
		retryMax = retryMin
	}
	return &replicaCircuit{probe: probe, log: log, now: time.Now, min: retryMin, max: retryMax}
}

// use reports whether the replica may serve the next read, probing it when the
// backoff window has expired. The probe runs under the mutex so a burst of
// concurrent reads issues one probe between them rather than one each.
func (c *replicaCircuit) use() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tripped {
		return true
	}
	if c.now().Before(c.retryAt) {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), replicaProbeTimeout)
	defer cancel()
	if err := c.probe.Ping(ctx); err != nil {
		c.log.Debug("postgres replica still unhealthy", "err", err)
		c.backOffLocked()
		return false
	}
	c.log.Info("postgres replica healthy again, reads return to it")
	c.tripped = false
	c.backoff = 0
	return true
}

// trip marks the replica unhealthy. An already-tripped circuit keeps the window
// it has, so a flood of reported errors cannot push the retry out indefinitely.
func (c *replicaCircuit) trip(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tripped {
		return
	}
	c.log.Warn("postgres replica unhealthy, reads move to the primary", "err", err)
	c.backOffLocked()
}

func (c *replicaCircuit) backOffLocked() {
	c.tripped = true
	switch c.backoff {
	case 0:
		c.backoff = c.min
	default:
		c.backoff = min(c.backoff*2, c.max)
	}
	c.retryAt = c.now().Add(c.backoff)
}
