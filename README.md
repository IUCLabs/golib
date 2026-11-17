# golib

Batteries for Go services at IUCLabs.

## Packages

| Import | What it does |
| --- | --- |
| `github.com/IUCLabs/golib/pg` | Postgres: DSN from the environment, pgxpool construction, read/write splitting across a primary and a replica, and an advisory-locked migration runner. |
| `github.com/IUCLabs/golib/pg/pgtest` | A throwaway Postgres container for your own `_test.go` files. Test-only. |
| `github.com/IUCLabs/golib/keyring` | Encrypts secrets at rest with a key held in the Linux kernel user keyring, scoped to the current boot and uid. |
| `github.com/IUCLabs/golib/colour` | jq-style JSON and yq-style YAML highlighting for CLI output, preserving the input byte for byte, plus the TTY/`NO_COLOR` check. |

```
go get github.com/IUCLabs/golib@latest
```

## Usage

### pg

```go
dsn, err := pg.DSNFromEnv("MYAPP_") // MYAPP_DATABASE_URL, MYAPP_DBHOST, ..., then PG*
if err != nil {
    return err
}
pool, err := pg.NewMigrated(ctx, dsn, migrations.FS, pg.MigrateOptions{
    LockName: "myapp-migrations",
    Logger:   log,
})
```

Split reads to a replica with `pg.Cluster`; routing is explicit and an
unhealthy or missing replica falls back to the primary:

```go
primaryDSN, replicaDSN, err := pg.ClusterDSNsFromEnv("MYAPP_") // replica from MYAPP_DB_RO_HOST
db, err := pg.NewCluster(ctx, pg.ClusterConfig{PrimaryDSN: primaryDSN, ReplicaDSN: replicaDSN})
defer db.Close()

rows, err := db.Read().Query(ctx, "SELECT id, name FROM widgets") // replica when healthy
tag, err := db.Write().Exec(ctx, "UPDATE widgets SET ...")        // always the primary
```

### pgtest

Starts `postgres:17-alpine` via testcontainers (needs Docker); skips under `-short`.

```go
func TestSomething(t *testing.T) {
    dsn := pgtest.MustStartPostgres(context.Background(), t)
    ...
}
```

### keyring

```go
kr, err := keyring.Open("golib:mytool") // get-or-create the key
sealed, err := kr.Seal(token)
token, err := kr.Unseal(sealed)
if errors.Is(err, keyring.ErrKeyUnavailable) {
    // rebooted or Forget()-ed: log in again
}
```

Linux only; other platforms return `errors.ErrUnsupported`.

### colour

```go
if colour.Enabled(colour.Auto, os.Stdout) {
    err = colour.JSON(os.Stdout, data)
} else {
    _, err = os.Stdout.Write(data)
}
```

## Requirements

Go 1.25 or later.

## License

MIT — see [LICENSE](LICENSE).
