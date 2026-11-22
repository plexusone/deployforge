package dbcopy

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grokify/pgutil"

	"github.com/plexusone/deployforge/dbroles"
)

// pgServer is a throwaway PostgreSQL server on a Unix socket, for testing
// the copy with PostgreSQL's own programs. It needs DEPLOYFORGE_PG_INTEGRATION=1
// and the programs in DEPLOYFORGE_PG_BIN or on PATH.
type pgServer struct {
	bin, sock string
	port      int
}

func newPGServer(t *testing.T) *pgServer {
	t.Helper()
	if os.Getenv("DEPLOYFORGE_PG_INTEGRATION") != "1" {
		t.Skip("set DEPLOYFORGE_PG_INTEGRATION=1, and DEPLOYFORGE_PG_BIN to PostgreSQL's bin directory, to copy between real databases")
	}
	bin := os.Getenv("DEPLOYFORGE_PG_BIN")
	if bin == "" {
		p, err := exec.LookPath("initdb")
		if err != nil {
			t.Skip("initdb not found; set DEPLOYFORGE_PG_BIN")
		}
		bin = filepath.Dir(p)
	}
	sock, err := os.MkdirTemp("/tmp", "dbcopy") // a socket's path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sock); err != nil {
			t.Errorf("removing %s: %v", sock, err)
		}
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	s := &pgServer{bin: bin, sock: sock, port: port}
	data := t.TempDir()
	s.run(t, "", "initdb", "-D", data, "-U", "boot", "--auth=trust", "-E", "UTF8", "--locale=C")
	s.run(t, "", "pg_ctl", "-D", data, "-w", "-l", filepath.Join(data, "log"), "-o",
		fmt.Sprintf("-p %d -k %s -c listen_addresses=''", port, sock), "start")
	t.Cleanup(func() { s.run(t, "", "pg_ctl", "-D", data, "-w", "-m", "fast", "stop") })
	return s
}

func (s *pgServer) run(t *testing.T, stdin, prog string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(s.bin, prog), args...) //nolint:gosec // G204: PostgreSQL's own programs, from DEPLOYFORGE_PG_BIN or PATH
	cmd.Env = append(os.Environ(), "LC_ALL=C")               // the server refuses to start on macOS without a valid locale
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", prog, err, out.String())
	}
	return out.String()
}

func (s *pgServer) psql(t *testing.T, user, db, sql string) string {
	t.Helper()
	return s.run(t, sql, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-h", s.sock, "-p", fmt.Sprint(s.port), "-U", user, "-d", db)
}

// dsn names a database on the socket, as a postgres:// URL.
func (s *pgServer) dsn(user, db string) string {
	return pgutil.DSN{User: user, Host: "localhost", Port: s.port, Database: db}.String() + "?host=" + s.sock
}

// local runs the via host's commands here, with PostgreSQL's programs on
// PATH: bash -c in place of ssh.
func (s *pgServer) local() Remote {
	return dbroles.Remote{SSH: []string{"env", "PATH=" + s.bin + ":" + os.Getenv("PATH"), "LC_ALL=C", "bash", "-c"}}
}

func TestCopyBetweenRealDatabases(t *testing.T) {
	s := newPGServer(t)
	ctx := context.Background()
	s.psql(t, "boot", "postgres", `
CREATE ROLE old_owner LOGIN; CREATE ROLE new_owner LOGIN; CREATE ROLE src_app LOGIN;
CREATE DATABASE src OWNER old_owner; CREATE DATABASE dst OWNER new_owner;`)
	s.psql(t, "old_owner", "src", `
CREATE TABLE accounts (id serial PRIMARY KEY, email text UNIQUE NOT NULL);
CREATE TABLE sessions (id bigserial PRIMARY KEY, account int REFERENCES accounts(id), data jsonb);
INSERT INTO accounts (email) SELECT 'u' || g || '@example.com' FROM generate_series(1, 250) g;
INSERT INTO sessions (account, data) SELECT 1 + g % 250, jsonb_build_object('n', g) FROM generate_series(1, 1000) g;
-- Row-level security that binds the owner too, as a multi-tenant store's does.
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON sessions USING (account = current_setting('app.account', true)::int);
-- The app role's grants: dropped by the copy, given again by migrate.
GRANT SELECT, INSERT ON accounts TO src_app;`)

	copies := []Copy{{Database: "dst", Source: s.dsn("old_owner", "src"), Target: s.dsn("new_owner", "dst"), AppRole: "src_app"}}
	for i := range 2 { // again: the restore replaces what the target has
		if err := Run(ctx, copies, s.local()); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// What a newer release's migrate made in the target, which the
			// source lacks: the next copy drops it all.
			s.psql(t, "new_owner", "dst", `
CREATE SCHEMA extra; CREATE TABLE extra.t (id int);
CREATE TABLE newer (id serial PRIMARY KEY, account int REFERENCES accounts(id));
CREATE VIEW newer_v AS SELECT id FROM newer;
CREATE TYPE mood AS ENUM ('ok'); CREATE DOMAIN pos AS int CHECK (VALUE > 0);
CREATE FUNCTION newer_f() RETURNS int LANGUAGE sql AS 'SELECT 1';
CREATE SEQUENCE loose;`)
		}
	}
	left := s.psql(t, "boot", "dst", `SELECT string_agg(x, ',') FROM (
  SELECT relname::text AS x FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname IN ('newer', 'newer_v', 'loose')
  UNION ALL SELECT nspname FROM pg_namespace WHERE nspname = 'extra'
  UNION ALL SELECT typname FROM pg_type WHERE typname IN ('mood', 'pos')
  UNION ALL SELECT proname FROM pg_proc WHERE proname = 'newer_f') s;`)
	if strings.TrimSpace(left) != "" {
		t.Errorf("the copy left what only the target had: %s", left)
	}
	src, err := Counts(ctx, copies[0].Source, s.local())
	if err != nil {
		t.Fatal(err)
	}
	dst, err := Counts(ctx, copies[0].Target, s.local())
	if err != nil {
		t.Fatal(err)
	}
	// Every row is counted and copied, although the policy binds the owner.
	if d := CompareCounts(src, dst); len(d) != 0 || src["public.sessions"] != 1000 || len(src) != 2 {
		t.Fatalf("counts: %v; source %v", d, src)
	}
	if n := strings.TrimSpace(s.psql(t, "boot", "dst", "SELECT count(*) FROM sessions;")); n != "1000" {
		t.Errorf("the target has %s sessions, want 1000", n)
	}
	for _, db := range []string{"src", "dst"} {
		if rls := strings.TrimSpace(s.psql(t, "boot", db, "SELECT relforcerowsecurity FROM pg_class WHERE relname = 'sessions';")); rls != "t" {
			t.Errorf("%s: row-level security is not forced after the copy: %q", db, rls)
		}
	}
	owners := s.psql(t, "boot", "dst", "SELECT DISTINCT tableowner FROM pg_tables WHERE schemaname = 'public';")
	if strings.TrimSpace(owners) != "new_owner" {
		t.Errorf("restored tables are owned by %q", owners)
	}
	// The sequences carried on: a new row does not collide.
	s.psql(t, "new_owner", "dst", "INSERT INTO accounts (email) VALUES ('new@example.com');")

	s.psql(t, "boot", "src", "INSERT INTO accounts (email) VALUES ('a@example.com'), ('b@example.com');")
	src, err = Counts(ctx, copies[0].Source, s.local())
	if err != nil {
		t.Fatal(err)
	}
	if d := CompareCounts(src, dst); len(d) != 1 || !strings.Contains(d[0], "public.accounts: 252 rows in the source, 250 in the target") {
		t.Errorf("a changed source: %v", d)
	}

	// A table the source's role cannot read is listed, never left out.
	s.psql(t, "boot", "src", "CREATE TABLE secret (id int); INSERT INTO secret VALUES (1);")
	src, err = Counts(ctx, copies[0].Source, s.local())
	if err != nil {
		t.Fatal(err)
	}
	if src["public.secret"] != Unreadable {
		t.Errorf("an unreadable table: %v", src)
	}
	if d := CompareCounts(src, dst); !strings.Contains(strings.Join(d, "\n"), "public.secret: the source's role cannot read it") {
		t.Errorf("an unreadable table was not reported: %v", d)
	}

	// A failing copy leaves the target as it was, and the source forced.
	bad := []Copy{{Database: "dst", Source: s.dsn("old_owner", "missing"), Target: s.dsn("new_owner", "dst")}}
	if err := Run(ctx, bad, s.local()); err == nil {
		t.Error("a copy from a missing database succeeded")
	}
	if n := strings.TrimSpace(s.psql(t, "boot", "dst", "SELECT count(*) FROM accounts;")); n != "251" {
		t.Errorf("after a failed copy the target has %s accounts", n)
	}
	// A copy that fails part-way, after lifting FORCE, puts it back.
	s.psql(t, "boot", "postgres", "CREATE DATABASE locked OWNER boot;")
	half := []Copy{{Database: "locked", Source: s.dsn("old_owner", "src"), Target: s.dsn("old_owner", "locked")}}
	if err := Run(ctx, half, s.local()); err == nil {
		t.Error("a copy into a database its role cannot write succeeded")
	}
	if rls := strings.TrimSpace(s.psql(t, "boot", "src", "SELECT relforcerowsecurity FROM pg_class WHERE relname = 'sessions';")); rls != "t" {
		t.Errorf("a failed copy left the source's row-level security lifted: %q", rls)
	}

	// A schema that needs its own roles is refused before anything changes:
	// a restore with no owners or grants would loosen it.
	s.psql(t, "boot", "postgres", "CREATE ROLE resolver NOLOGIN; CREATE DATABASE shared OWNER old_owner; CREATE DATABASE shared_new OWNER new_owner;")
	s.psql(t, "boot", "shared", `
SET ROLE old_owner;
CREATE TABLE bindings (id int);
ALTER TABLE bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bindings FORCE ROW LEVEL SECURITY;
RESET ROLE;
CREATE POLICY resolver_read ON bindings FOR SELECT TO resolver USING (true);
CREATE FUNCTION resolve() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT count(*)::int FROM bindings';
ALTER FUNCTION resolve() OWNER TO resolver;
REVOKE ALL ON FUNCTION resolve() FROM PUBLIC;`)
	shared := []Copy{{Database: "shared_new", Source: s.dsn("old_owner", "shared"), Target: s.dsn("new_owner", "shared_new")}}
	err = Run(ctx, shared, s.local())
	for _, want := range []string{"db copy refuses this database", "function resolve() is owned by resolver", "function resolve() does not let PUBLIC execute it", "policy resolver_read on bindings names role resolver"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the guard did not report %q: %v", want, err)
		}
	}
	if n := strings.TrimSpace(s.psql(t, "boot", "shared_new", "SELECT count(*) FROM pg_tables WHERE schemaname = 'public';")); n != "0" {
		t.Errorf("a refused copy wrote %s tables", n)
	}
	if rls := strings.TrimSpace(s.psql(t, "boot", "shared", "SELECT relforcerowsecurity FROM pg_class WHERE relname = 'bindings';")); rls != "t" {
		t.Errorf("a refused copy changed the source: %q", rls)
	}
}
