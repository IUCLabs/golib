# golib

Shared Go library for the unkin estate: the plumbing that was being copy-pasted
between services, kept in one place with one set of tests.

golib is a library only. It ships no binaries and no container images, holds no
service configuration, and takes on dependencies grudgingly — every service that
imports it inherits them.

## Modules

| Import | What it does |
| --- | --- |
| `git.unkin.net/unkin/golib/pg` | Postgres: DSN from the environment, pgxpool construction, read/write splitting across a primary and a replica, and the estate's migration runner. |
| `git.unkin.net/unkin/golib/pg/pgtest` | A throwaway Postgres container for a consumer's own `_test.go` files. Test-only. |
| `git.unkin.net/unkin/golib/keyring` | Encrypts secrets at rest with a key held in the Linux kernel user keyring, scoped to the current boot and uid. |

### pg

```go
dsn, err := pg.DSNFromEnv("ENCAPI_")
if err != nil {
    return err
}
pool, err := pg.NewMigrated(ctx, dsn, migrations.FS, pg.MigrateOptions{
    LockName: "encapi-migrations",
    Logger:   log,
})
```

`pg.DSNFromEnv(prefix)` resolves a connection string from the environment.
Precedence, highest first:

1. `<PREFIX>DATABASE_URL` — used verbatim.
2. `DATABASE_URL` — likewise.
3. `<PREFIX>DBHOST`, `<PREFIX>DBPORT`, `<PREFIX>DBUSER`, `<PREFIX>DBPASS`,
   `<PREFIX>DBNAME`, `<PREFIX>DBSSL`.
4. libpq's `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`, `PGSSLMODE`.
5. Defaults: `localhost`, `5432`, `sslmode=disable`.

Levels 3 to 5 resolve per field, so a deployment can set the password from a
secret and leave the rest to `PG*`. User and database name have no default —
an unset one is an error naming the variables that were checked. An empty prefix
reads the bare `DBHOST`/`DBPORT`/… names the estate's services already use, so
the rendered DSN is unchanged from the `fmt.Sprintf` builders this replaces.

`pg.New` opens a pgxpool and pings it, so an unreachable server fails at startup
rather than on the first query. `pg.NewMigrated` does that and then migrates.

`pg.Migrate(ctx, pool, fsys, opts)` applies the `.sql` files at the root of
`fsys` in lexical filename order. Every replica calls it at startup:

- The run holds a cluster-wide `pg_advisory_lock` keyed on FNV-1a/64 of
  `opts.LockName`, on one dedicated pooled connection, because the lock is
  session-scoped. Replicas that queue behind the winner find the set already
  recorded and do nothing.
- Each file is applied together with its `schema_migrations` row in a single
  transaction, so a failure leaves neither a half-tracked migration nor a
  tracking row that would skip it next time.
- A file missing from `schema_migrations` is re-applied even if the live
  database already has it, which is how a schema applied out of band is adopted.
  Write migrations `IF NOT EXISTS`-guarded so that re-run is a no-op.
- If the unlock does not land, the connection is discarded rather than returned
  to the pool, so a session that may still hold the lock cannot be reused.

`pg.LockKey(name)` exposes the derivation, so a service migrating off a
hardcoded key can assert the two agree before switching over.

#### Read/write splitting

`pg.Cluster` wraps a primary pool and an optional read-replica pool:

```go
primaryDSN, replicaDSN, err := pg.ClusterDSNsFromEnv("ENCAPI_")
if err != nil {
    return err
}
db, err := pg.NewCluster(ctx, pg.ClusterConfig{
    PrimaryDSN: primaryDSN,
    ReplicaDSN: replicaDSN,
    Logger:     log,
})
if err != nil {
    return err
}
defer db.Close()

if err := db.Migrate(ctx, migrations.FS, pg.MigrateOptions{LockName: "encapi-migrations"}); err != nil {
    return err
}

rows, err := db.Read().Query(ctx, "SELECT id, name FROM nodes")   // replica when healthy
tag, err := db.Write().Exec(ctx, "UPDATE nodes SET ...")          // always the primary
row := db.Primary().QueryRow(ctx, "SELECT ... WHERE id = $1", id) // read-your-writes
```

| Call | Goes to |
| --- | --- |
| `Write()` | The primary, always. |
| `Read()` | The replica when one is configured and healthy; the primary otherwise. |
| `Primary()` | The primary. Same pool as `Write()`, named for reads that must not be stale. |

**Routing is explicit; nothing inspects SQL.** Deciding from the statement text
gets it wrong in both directions: a CTE with an `INSERT` in it reads as a
`SELECT`, and `SELECT ... FOR UPDATE` takes row locks a replica cannot grant.
The caller knows which it wants, so the caller picks.

**Replica lag is real.** A replica serves a slightly stale snapshot, so a read
that has to observe a write this process just made goes to `Primary()`. The
usual shape is a handler that writes and then re-reads what it wrote, or a
redirect straight into a `GET` of the row just created — both need the primary.
Everything else (list endpoints, dashboards, reports, background aggregation)
can take the replica.

**A missing replica is not an error.** With `ReplicaDSN` empty, `Read()` returns
the primary and the `Cluster` is an ordinary single-pool handle, so a service
can use `Cluster` unconditionally and let the deployment decide whether reads
split. A `ReplicaDSN` equal to `PrimaryDSN` does the same rather than opening a
second pool to the same place.

**An unhealthy replica falls back.** The `Cluster` keeps a small circuit over
the replica:

- A replica that fails its startup ping does not fail startup; reads begin on
  the primary and move over once it answers a probe.
- `db.ReportReplicaError(err)` — pass the error from a query run on the pool
  `Read()` handed out — trips the circuit, and reads move to the primary.
- While tripped, `Read()` issues no probes until the backoff window expires; the
  next `Read()` after that pays for one probe that either promotes the replica
  or doubles the window. The window runs from `ReplicaRetryMin` to
  `ReplicaRetryMax` (5s to 2m by default) and resets on recovery. A healthy
  replica is never probed at all, so the split costs nothing on the read path.
- Reporting is advisory. A caller that never reports still routes correctly; it
  just does not react to a replica that dies mid-flight.

**Migrations always target the primary.** `Cluster.Migrate` runs through
`Write()`. A replica is physically read-only, and a schema change has to
originate on the primary to reach the replica at all.

`pg.ClusterDSNsFromEnv(prefix)` returns both connection strings. The primary
follows `DSNFromEnv` exactly. The replica resolves, highest first:

1. `<PREFIX>DATABASE_RO_URL` — used verbatim.
2. `DATABASE_RO_URL` — likewise.
3. `<PREFIX>DB_RO_HOST`, or bare `DB_RO_HOST` — the primary's port, user,
   password, database and sslmode with that host substituted.
4. Nothing set — an empty replica DSN, so the `Cluster` runs single-pool.

Nothing is derived. The read-only host is never rewritten out of the primary's,
because a wrong guess silently sends reads somewhere unintended. Setting only
`DB_RO_HOST` while the primary comes from a whole `DATABASE_URL` is an error,
not a guess: the fields to substitute into are not known.

CloudNativePG publishes exactly the two endpoints this expects — `<cluster>-rw`
routes to the primary and `<cluster>-ro` to the replicas — so a Deployment names
both:

```yaml
env:
  - name: ENCAPI_DBHOST
    value: encapi-db-rw
  - name: ENCAPI_DB_RO_HOST
    value: encapi-db-ro
  - name: ENCAPI_DBNAME
    value: encapi
  - name: ENCAPI_DBUSER
    valueFrom: { secretKeyRef: { name: encapi-db-app, key: username } }
  - name: ENCAPI_DBPASS
    valueFrom: { secretKeyRef: { name: encapi-db-app, key: password } }
```

Dropping `ENCAPI_DB_RO_HOST` turns the split off without a code change.

### pgtest

`pgtest` starts `postgres:17-alpine` via testcontainers. Import it only from
`_test.go` files. The estate's CI runs on Kubernetes with no Docker socket, so
container-backed tests must skip themselves under `-short`:

```go
func TestSomething(t *testing.T) {
    ctx := context.Background()
    dsn := pgtest.MustStartPostgres(ctx, t) // skips under -short, cleans up after
    ...
}
```

### keyring

```go
kr, err := keyring.Open("golib:vault-tools") // get-or-create the key
sealed, err := kr.Seal(token)
token, err := kr.Unseal(sealed)
if errors.Is(err, keyring.ErrKeyUnavailable) {
    // rebooted or Forget()-ed: log in again
}
```

The key is 32 random bytes stored as a `user` key in the uid's user keyring
(`@u`), so every login shell, terminal and systemd user service of that uid
shares it until reboot. Its permissions grant the uid read access as well as the
possessor, because processes whose session keyring does not link `@u` do not
possess it. Data is AES-256-GCM in a versioned envelope; a missing, revoked or
different key returns `ErrKeyUnavailable`, damaged data returns `ErrCorrupt`.
`Forget` revokes the key. Non-Linux builds return `errors.ErrUnsupported`.

## Consuming

golib is versioned with semver tags and consumed like any Go module. Pin a tag:

```
go get git.unkin.net/unkin/golib@v0.1.0
```

Nothing is released until a `v*` tag exists; `make patch`, `make minor` and
`make major` cut and push the next one.

Because everything shares one module path, a consumer that imports only `pg`
still resolves golib's full dependency set in its module graph. That is the
reason to keep the dependency list short, and the reason `pgtest`'s
testcontainers dependency is the exception rather than the pattern.

## Development

```
make build     # compile every package
make test      # unit tests (-short: no container needed)
make test-all  # everything, including the container-backed integration tests
make cover     # unit tests with the coverage gate
make lint      # golangci-lint
```

New code needs meaningful tests. `make cover` fails below **90% statement
coverage**, measured over the shipped packages from the unit tests alone — the
integration tests do not count towards it, so the bar has to be cleared without
a database. `pg/pgtest` is excluded: it is test scaffolding for other repos, and
is covered by the integration tests the gate does not run.

CI runs `test`, `pre-commit` and `build` on every pull request.
