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
)

// DSNFromEnv builds a libpq/pgx connection string from the environment.
//
// prefix is prepended to every custom variable name, so a service can namespace
// its settings ("ENCAPI_" reads ENCAPI_DBHOST); an empty prefix reads the bare
// names the estate's services already use (DBHOST, DBPORT, ...).
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
	if v := os.Getenv(prefix + "DATABASE_URL"); v != "" {
		return v, nil
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v, nil
	}

	host := lookup(prefix, fieldHost, defaultHost)
	portStr := lookup(prefix, fieldPort, strconv.Itoa(defaultPort))
	user := lookup(prefix, fieldUser, "")
	pass := lookup(prefix, fieldPass, "")
	name := lookup(prefix, fieldName, "")
	ssl := lookup(prefix, fieldSSL, defaultSSLMode)

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("invalid %s%s: %w", prefix, fieldPort.suffix, err)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid %s%s: port %d out of range", prefix, fieldPort.suffix, port)
	}
	if user == "" {
		return "", fmt.Errorf("no database user: set %s%s or %s", prefix, fieldUser.suffix, fieldUser.libpq)
	}
	if name == "" {
		return "", fmt.Errorf("no database name: set %s%s or %s", prefix, fieldName.suffix, fieldName.libpq)
	}

	return DSN(host, port, user, pass, name, ssl), nil
}

// DSN renders the estate's standard connection string. User and password are
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
	if v := os.Getenv(prefix + f.suffix); v != "" {
		return v
	}
	if v := os.Getenv(f.libpq); v != "" {
		return v
	}
	return def
}
