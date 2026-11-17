package pg

import (
	"net/url"
	"strings"
	"testing"
)

// dsnVars is every variable DSNFromEnv reads, for both the bare and the
// prefixed namespace used by these tests. Each case starts from all of them
// unset so an inherited PGHOST on a developer's machine cannot change a result.
var dsnVars = []string{
	"DATABASE_URL", "DBHOST", "DBPORT", "DBUSER", "DBPASS", "DBNAME", "DBSSL",
	"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE",
	"APP_DATABASE_URL", "APP_DBHOST", "APP_DBPORT", "APP_DBUSER", "APP_DBPASS",
	"APP_DBNAME", "APP_DBSSL",
	"DATABASE_RO_URL", "DB_RO_HOST", "APP_DATABASE_RO_URL", "APP_DB_RO_HOST",
}

// setEnv clears every variable DSNFromEnv consults, then sets the given ones.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range dsnVars {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestDSNFromEnv(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		env    map[string]string
		want   string
	}{
		{
			name:   "prefixed custom vars",
			prefix: "APP_",
			env: map[string]string{
				"APP_DBHOST": "db.internal", "APP_DBPORT": "6432",
				"APP_DBUSER": "app", "APP_DBPASS": "s3cret",
				"APP_DBNAME": "appdb", "APP_DBSSL": "require",
			},
			want: "postgres://app:s3cret@db.internal:6432/appdb?sslmode=require",
		},
		{
			name:   "bare custom vars with an empty prefix",
			prefix: "",
			env: map[string]string{
				"DBHOST": "pg", "DBPORT": "5432", "DBUSER": "myapp",
				"DBPASS": "myapp", "DBNAME": "myapp", "DBSSL": "disable",
			},
			// Byte-identical to the fmt.Sprintf builder this replaces.
			want: "postgres://myapp:myapp@pg:5432/myapp?sslmode=disable",
		},
		{
			name:   "libpq vars fill in",
			prefix: "APP_",
			env: map[string]string{
				"PGHOST": "libpq.host", "PGPORT": "5433", "PGUSER": "pguser",
				"PGPASSWORD": "pgpass", "PGDATABASE": "pgdb", "PGSSLMODE": "verify-full",
			},
			want: "postgres://pguser:pgpass@libpq.host:5433/pgdb?sslmode=verify-full",
		},
		{
			name:   "prefixed vars beat libpq vars per field",
			prefix: "APP_",
			env: map[string]string{
				"APP_DBPASS": "from-secret",
				"PGHOST":     "libpq.host", "PGUSER": "pguser",
				"PGPASSWORD": "ignored", "PGDATABASE": "pgdb",
			},
			want: "postgres://pguser:from-secret@libpq.host:5432/pgdb?sslmode=disable",
		},
		{
			name:   "defaults for host, port and sslmode",
			prefix: "",
			env:    map[string]string{"DBUSER": "u", "DBNAME": "d"},
			want:   "postgres://u:@localhost:5432/d?sslmode=disable",
		},
		{
			name:   "prefixed DATABASE_URL passes through verbatim",
			prefix: "APP_",
			env: map[string]string{
				"APP_DATABASE_URL": "postgres://who:cares@elsewhere/db?sslmode=require&application_name=x",
				"APP_DBHOST":       "ignored", "APP_DBUSER": "ignored", "APP_DBNAME": "ignored",
			},
			want: "postgres://who:cares@elsewhere/db?sslmode=require&application_name=x",
		},
		{
			name:   "bare DATABASE_URL wins over the field vars",
			prefix: "APP_",
			env: map[string]string{
				"DATABASE_URL": "postgres://u:p@h:5432/d",
				"APP_DBHOST":   "ignored", "APP_DBUSER": "ignored", "APP_DBNAME": "ignored",
			},
			want: "postgres://u:p@h:5432/d",
		},
		{
			name:   "prefixed DATABASE_URL wins over the bare one",
			prefix: "APP_",
			env: map[string]string{
				"APP_DATABASE_URL": "postgres://app@app-host/app",
				"DATABASE_URL":     "postgres://bare@bare-host/bare",
			},
			want: "postgres://app@app-host/app",
		},
		{
			name:   "reserved characters in the password are escaped",
			prefix: "",
			env: map[string]string{
				"DBHOST": "h", "DBUSER": "u", "DBPASS": "p@ss/w:rd", "DBNAME": "d",
			},
			want: "postgres://u:p%40ss%2Fw%3Ard@h:5432/d?sslmode=disable",
		},
		{
			name:   "IPv6 host is bracketed",
			prefix: "",
			env: map[string]string{
				"DBHOST": "fd00::1", "DBUSER": "u", "DBNAME": "d",
			},
			want: "postgres://u:@[fd00::1]:5432/d?sslmode=disable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			got, err := DSNFromEnv(tc.prefix)
			if err != nil {
				t.Fatalf("DSNFromEnv: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DSNFromEnv = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every DSN this builds must survive the parse pgx will do on it.
func TestDSNFromEnv_ResultParses(t *testing.T) {
	setEnv(t, map[string]string{
		"DBHOST": "fd00::1", "DBPORT": "6432", "DBUSER": "us er",
		"DBPASS": "p@ss/w:rd", "DBNAME": "app", "DBSSL": "verify-full",
	})
	dsn, err := DSNFromEnv("")
	if err != nil {
		t.Fatalf("DSNFromEnv: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %q: %v", dsn, err)
	}
	if u.Hostname() != "fd00::1" {
		t.Errorf("host = %q, want fd00::1", u.Hostname())
	}
	if u.Port() != "6432" {
		t.Errorf("port = %q, want 6432", u.Port())
	}
	if u.User.Username() != "us er" {
		t.Errorf("user = %q, want %q", u.User.Username(), "us er")
	}
	pass, _ := u.User.Password()
	if pass != "p@ss/w:rd" {
		t.Errorf("password = %q, want %q", pass, "p@ss/w:rd")
	}
	if got := strings.TrimPrefix(u.Path, "/"); got != "app" {
		t.Errorf("database = %q, want app", got)
	}
	if got := u.Query().Get("sslmode"); got != "verify-full" {
		t.Errorf("sslmode = %q, want verify-full", got)
	}
}

func TestDSNFromEnv_Errors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantSub string
	}{
		{
			name:    "unparseable port",
			env:     map[string]string{"DBPORT": "not-a-port", "DBUSER": "u", "DBNAME": "d"},
			wantSub: "invalid DBPORT",
		},
		{
			name:    "port out of range",
			env:     map[string]string{"DBPORT": "70000", "DBUSER": "u", "DBNAME": "d"},
			wantSub: "out of range",
		},
		{
			name:    "no user",
			env:     map[string]string{"DBNAME": "d"},
			wantSub: "set DBUSER or PGUSER",
		},
		{
			name:    "no database name",
			env:     map[string]string{"DBUSER": "u"},
			wantSub: "set DBNAME or PGDATABASE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			got, err := DSNFromEnv("")
			if err == nil {
				t.Fatalf("expected an error, got DSN %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// The error names the prefixed variable the caller is expected to set, not the
// bare one, or the message sends them looking for the wrong knob.
func TestDSNFromEnv_ErrorNamesPrefixedVar(t *testing.T) {
	setEnv(t, map[string]string{"APP_DBPORT": "x", "APP_DBUSER": "u", "APP_DBNAME": "d"})
	_, err := DSNFromEnv("APP_")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "APP_DBPORT") {
		t.Fatalf("error %q does not name APP_DBPORT", err)
	}
}

func TestClusterDSNsFromEnv(t *testing.T) {
	// The primary every field-based case resolves to, spelled once.
	const wantPrimary = "postgres://app:pw@db-rw:5432/appdb?sslmode=require"
	fieldEnv := map[string]string{
		"APP_DBHOST": "db-rw", "APP_DBUSER": "app", "APP_DBPASS": "pw",
		"APP_DBNAME": "appdb", "APP_DBSSL": "require",
	}
	withFields := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range fieldEnv {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}

	tests := []struct {
		name        string
		prefix      string
		env         map[string]string
		wantPrimary string
		wantReplica string
	}{
		{
			name:        "no replica variables leaves the replica empty",
			prefix:      "APP_",
			env:         withFields(nil),
			wantPrimary: wantPrimary,
			wantReplica: "",
		},
		{
			name:        "prefixed DB_RO_HOST substitutes only the host",
			prefix:      "APP_",
			env:         withFields(map[string]string{"APP_DB_RO_HOST": "db-ro"}),
			wantPrimary: wantPrimary,
			wantReplica: "postgres://app:pw@db-ro:5432/appdb?sslmode=require",
		},
		{
			name:        "bare DB_RO_HOST fills in for a prefixed lookup",
			prefix:      "APP_",
			env:         withFields(map[string]string{"DB_RO_HOST": "db-ro"}),
			wantPrimary: wantPrimary,
			wantReplica: "postgres://app:pw@db-ro:5432/appdb?sslmode=require",
		},
		{
			name:   "prefixed DB_RO_HOST wins over the bare one",
			prefix: "APP_",
			env: withFields(map[string]string{
				"APP_DB_RO_HOST": "db-ro", "DB_RO_HOST": "ignored",
			}),
			wantPrimary: wantPrimary,
			wantReplica: "postgres://app:pw@db-ro:5432/appdb?sslmode=require",
		},
		{
			name:   "DATABASE_RO_URL passes through verbatim and wins over the host",
			prefix: "APP_",
			env: withFields(map[string]string{
				"APP_DATABASE_RO_URL": "postgres://ro@ro-host/db?application_name=reader",
				"APP_DB_RO_HOST":      "ignored",
			}),
			wantPrimary: wantPrimary,
			wantReplica: "postgres://ro@ro-host/db?application_name=reader",
		},
		{
			name:   "prefixed DATABASE_RO_URL wins over the bare one",
			prefix: "APP_",
			env: withFields(map[string]string{
				"APP_DATABASE_RO_URL": "postgres://app-ro@app-ro-host/db",
				"DATABASE_RO_URL":     "postgres://bare-ro@bare-ro-host/db",
			}),
			wantPrimary: wantPrimary,
			wantReplica: "postgres://app-ro@app-ro-host/db",
		},
		{
			name:   "both endpoints as whole URLs",
			prefix: "",
			env: map[string]string{
				"DATABASE_URL":    "postgres://u:p@rw/db",
				"DATABASE_RO_URL": "postgres://u:p@ro/db",
			},
			wantPrimary: "postgres://u:p@rw/db",
			wantReplica: "postgres://u:p@ro/db",
		},
		{
			name:   "an unprefixed deployment reads the bare names",
			prefix: "",
			env: map[string]string{
				"DBHOST": "cnpg-rw", "DBUSER": "u", "DBNAME": "d",
				"DB_RO_HOST": "cnpg-ro",
			},
			wantPrimary: "postgres://u:@cnpg-rw:5432/d?sslmode=disable",
			wantReplica: "postgres://u:@cnpg-ro:5432/d?sslmode=disable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			primary, replica, err := ClusterDSNsFromEnv(tc.prefix)
			if err != nil {
				t.Fatalf("ClusterDSNsFromEnv: %v", err)
			}
			if primary != tc.wantPrimary {
				t.Errorf("primary = %q, want %q", primary, tc.wantPrimary)
			}
			if replica != tc.wantReplica {
				t.Errorf("replica = %q, want %q", replica, tc.wantReplica)
			}
		})
	}
}

func TestClusterDSNsFromEnv_Errors(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		env     map[string]string
		wantSub string
		// notSub, when set, must not appear in the error.
		notSub string
	}{
		{
			name:    "a broken primary is reported",
			prefix:  "",
			env:     map[string]string{"DBUSER": "u"},
			wantSub: "set DBNAME or PGDATABASE",
		},
		{
			// The fields to substitute the read-only host into are unknown, so
			// the alternative to an error is guessing at the connection.
			name:   "a host-only replica alongside a URL primary is ambiguous",
			prefix: "APP_",
			env: map[string]string{
				"APP_DATABASE_URL": "postgres://u:p@rw/db",
				"APP_DB_RO_HOST":   "db-ro",
			},
			wantSub: "APP_DB_RO_HOST is set but the primary comes from a connection URL: set APP_DATABASE_RO_URL instead",
		},
		{
			// The same ambiguity, but the host came from the bare variable the
			// prefixed lookup falls back to. The error must name the variable
			// the operator actually set: naming APP_DB_RO_HOST sends them
			// looking for something that is not in their environment.
			name:   "the bare replica host variable is named, not the prefixed one",
			prefix: "APP_",
			env: map[string]string{
				"APP_DATABASE_URL": "postgres://u:p@rw/db",
				"DB_RO_HOST":       "db-ro",
			},
			wantSub: "DB_RO_HOST is set but",
			notSub:  "APP_DB_RO_HOST",
		},
		{
			name:   "a broken port is reported when the replica is resolved",
			prefix: "",
			env: map[string]string{
				"DBUSER": "u", "DBNAME": "d", "DBPORT": "70000", "DB_RO_HOST": "ro",
			},
			wantSub: "out of range",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			primary, replica, err := ClusterDSNsFromEnv(tc.prefix)
			if err == nil {
				t.Fatalf("expected an error, got %q / %q", primary, replica)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
			if tc.notSub != "" && strings.Contains(err.Error(), tc.notSub) {
				t.Fatalf("error %q names %q, which is not the variable that was set", err, tc.notSub)
			}
		})
	}
}

func TestDSN_EmptyPasswordMatchesLegacyFormat(t *testing.T) {
	// The Sprintf builders rendered an unset password as an empty string
	// between the colon and the "@"; keep that shape so DSNs do not churn.
	if got, want := DSN("h", 5432, "u", "", "d", "disable"), "postgres://u:@h:5432/d?sslmode=disable"; got != want {
		t.Fatalf("DSN = %q, want %q", got, want)
	}
}
