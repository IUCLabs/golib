package pg

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Default connection settings applied when neither the prefixed nor the libpq
// variable for a field is set. User and database have no default: a service
// connecting to "postgres" as "postgres" is a bug, not a default.
const (
	defaultHost    = "localhost"
	defaultPort    = 5432
	defaultSSLMode = "disable"
)

// dsnField is one connection setting and the variable names it is read from.
type dsnField struct {
	// suffix is appended to the caller's prefix, e.g. "DBHOST".
	suffix string
	// libpq is the standard libpq variable for the same setting.
	libpq string
}

var (
	fieldHost = dsnField{"DBHOST", "PGHOST"}
	fieldPort = dsnField{"DBPORT", "PGPORT"}
	fieldUser = dsnField{"DBUSER", "PGUSER"}
	fieldPass = dsnField{"DBPASS", "PGPASSWORD"}
	fieldName = dsnField{"DBNAME", "PGDATABASE"}
	fieldSSL  = dsnField{"DBSSL", "PGSSLMODE"}

	// The replica host has no libpq counterpart; the second name is the bare
	// variable a prefixed lookup falls back to.
	fieldROHost = dsnField{"DB_RO_HOST", "DB_RO_HOST"}
)

// DSNFromEnv builds a libpq/pgx connection string from the environment.
//
// prefix is prepended to every custom variable name, so a service can namespace
// its settings ("MYAPP_" reads MYAPP_DBHOST); an empty prefix reads the bare
// names (DBHOST, DBPORT, ...).
//
// Precedence, highest first:
//
//  1. <PREFIX>DATABASE_URL — returned verbatim, no parsing or validation.
//  2. DATABASE_URL — likewise. Identical to 1 when prefix is empty.
//  3. <PREFIX>DBHOST, <PREFIX>DBPORT, <PREFIX>DBUSER, <PREFIX>DBPASS,
//     <PREFIX>DBNAME, <PREFIX>DBSSL.
//  4. libpq's PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE, PGSSLMODE.
//  5. Built-in defaults: localhost, 5432, sslmode=disable.
//
// Levels 3 to 5 resolve per field, so a deployment may set DBPASS from a secret
// and leave the rest to PG* variables. An unset variable and one set to the
// empty string are treated alike, except for the password, where the empty
// string is a legitimate value and only distinguishable from unset if set
// explicitly — both render the same DSN, so the distinction does not matter.
//
// User and database name have no default: DSNFromEnv reports an error naming
// the variables it looked at rather than connecting somewhere unintended.
func DSNFromEnv(prefix string) (string, error) {
	if v := urlFromEnv(prefix, "DATABASE_URL"); v != "" {
		return v, nil
	}
	p, err := connPartsFromEnv(prefix)
	if err != nil {
		return "", err
	}
	return p.dsn(), nil
}

// ClusterDSNsFromEnv resolves the primary and replica connection strings for a
// Cluster. The primary follows DSNFromEnv exactly; the replica resolves,
// highest first:
//
//  1. <PREFIX>DATABASE_RO_URL — used verbatim.
//  2. DATABASE_RO_URL — likewise.
//  3. <PREFIX>DB_RO_HOST, or bare DB_RO_HOST — the primary's port, user,
//     password, database and sslmode with that host substituted, which is the
//     CNPG shape where the <cluster>-rw and <cluster>-ro services differ only
//     in hostname.
//  4. Nothing set: an empty replica DSN, so the Cluster runs single-pool.
//
// Nothing is derived. A deployment that wants split reads names the read-only
// endpoint; the replica host is never rewritten out of the primary's, because a
// wrong guess silently sends reads somewhere unintended.
//
// A host-only replica variable set alongside a primary given as a whole URL is
// an error rather than a guess: the fields to substitute into are not known.
func ClusterDSNsFromEnv(prefix string) (primary, replica string, err error) {
	replica = urlFromEnv(prefix, "DATABASE_RO_URL")
	roHost, roHostVar := lookupNamed(prefix, fieldROHost, "")

	if u := urlFromEnv(prefix, "DATABASE_URL"); u != "" {
		if replica == "" && roHost != "" {
			return "", "", fmt.Errorf("%s is set but the primary comes from a connection URL: set %sDATABASE_RO_URL instead",
				roHostVar, prefix)
		}
		return u, replica, nil
	}

	p, err := connPartsFromEnv(prefix)
	if err != nil {
		return "", "", err
	}
	primary = p.dsn()
	if replica != "" || roHost == "" {
		return primary, replica, nil
	}
	p.host = roHost
	return primary, p.dsn(), nil
}

// urlFromEnv reads a whole-connection-string variable: prefixed, then bare.
func urlFromEnv(prefix, name string) string {
	if v := os.Getenv(prefix + name); v != "" {
		return v
	}
	return os.Getenv(name)
}

// connParts is one resolved set of connection fields.
type connParts struct {
	host string
	port int
	user string
	pass string
	name string
	ssl  string
}

func (p connParts) dsn() string { return DSN(p.host, p.port, p.user, p.pass, p.name, p.ssl) }

// connPartsFromEnv resolves levels 3 to 5 of DSNFromEnv's precedence.
func connPartsFromEnv(prefix string) (connParts, error) {
	portStr := lookup(prefix, fieldPort, strconv.Itoa(defaultPort))
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return connParts{}, fmt.Errorf("invalid %s%s: %w", prefix, fieldPort.suffix, err)
	}
	if port < 1 || port > 65535 {
		return connParts{}, fmt.Errorf("invalid %s%s: port %d out of range", prefix, fieldPort.suffix, port)
	}
	p := connParts{
		host: lookup(prefix, fieldHost, defaultHost),
		port: port,
		user: lookup(prefix, fieldUser, ""),
		pass: lookup(prefix, fieldPass, ""),
		name: lookup(prefix, fieldName, ""),
		ssl:  lookup(prefix, fieldSSL, defaultSSLMode),
	}
	if p.user == "" {
		return connParts{}, fmt.Errorf("no database user: set %s%s or %s", prefix, fieldUser.suffix, fieldUser.libpq)
	}
	if p.name == "" {
		return connParts{}, fmt.Errorf("no database name: set %s%s or %s", prefix, fieldName.suffix, fieldName.libpq)
	}
	return p, nil
}

// DSN renders the standard connection string. User and password are
// percent-escaped, so a password containing "@" or "/" does not truncate the
// host; for values without reserved characters the result is byte-identical to
// the fmt.Sprintf builders this replaces.
func DSN(host string, port int, user, pass, name, sslMode string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     hostPort(host, port),
		Path:     "/" + name,
		RawQuery: "sslmode=" + url.QueryEscape(sslMode),
	}
	return u.String()
}

// hostPort joins host and port, bracketing a bare IPv6 literal so the colons in
// the address are not read as the port separator.
func hostPort(host string, port int) string {
	if host != "" && host[0] != '[' && strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// lookup resolves one field: prefixed variable, then libpq variable, then def.
func lookup(prefix string, f dsnField, def string) string {
	v, _ := lookupNamed(prefix, f, def)
	return v
}

// lookupNamed resolves one field and reports the variable the value came from,
// empty when the default was used. An error message names the variable the
// operator actually set rather than the prefixed form they may never have used.
func lookupNamed(prefix string, f dsnField, def string) (value, name string) {
	if v := os.Getenv(prefix + f.suffix); v != "" {
		return v, prefix + f.suffix
	}
	if v := os.Getenv(f.libpq); v != "" {
		return v, f.libpq
	}
	return def, ""
}
