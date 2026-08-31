# golib

Shared Go library for the unkin estate: the plumbing that was being copy-pasted
between services, kept in one place with one set of tests.

golib is a library only. It ships no binaries and no container images, holds no
service configuration, and takes on dependencies grudgingly — every service that
imports it inherits them.

## Modules

| Import | What it does |
| --- | --- |
| `git.unkin.net/unkin/golib/pg` | Postgres: DSN from the environment, pgxpool construction, and the estate's migration runner. |
| `git.unkin.net/unkin/golib/pg/pgtest` | A throwaway Postgres container for a consumer's own `_test.go` files. Test-only. |

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
