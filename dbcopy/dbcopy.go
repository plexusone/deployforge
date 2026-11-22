// Package dbcopy moves an app stack's databases from one PostgreSQL server
// to another. The new server's roles and parameters are made first with
// dbroles under the stack's staged directory; Plan and Run then copy each
// database, on a fleet host, from the live server to the staged one; Counts
// and CompareCounts check every table's rows; and Promote switches the live
// parameters to the staged values, keeping the old ones under Previous so
// the old server can still be read until it is retired.
//
// The connections are secrets. They are read from the parameter store into
// memory and reach the host on ssh's standard input, never as arguments.
package dbcopy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/grokify/pgutil"

	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/fleet"
)

// Where a stack's parameters are, under its prefix.
const (
	// Live parameters are what deploys read.
	Live = ""
	// Staged parameters wait for a move: dbroles.StagedDir.
	Staged = dbroles.StagedDir
	// Previous keeps the live parameters Promote replaced.
	Previous = "previous/"
)

// Dirs are the directories by the names the command line uses.
var Dirs = map[string]string{"live": Live, "next": Staged, "previous": Previous}

// MigrateDir is the directory of migrate-only parameters. An owner
// connection kept there is the migrate step's base connection, which may
// name another database on the server: a copy switches it to its own.
const MigrateDir = "migrate/"

// Store is a stack's parameters. Get and Put are dbroles'; Set creates a
// parameter or replaces its value.
type Store interface {
	dbroles.Store
	Set(ctx context.Context, name, value string) error
}

// Copy is one database to copy: its name and the owner connections to read
// it and write it. The connections are secrets.
type Copy struct {
	Database       string
	Source, Target string
	// AppRole is the database's app role, whose grants the copy drops and
	// the app's migrate grants again; "" when it has none.
	AppRole string
	// Notes say how a connection was read, such as a base connection
	// switched to the database; they hold no secret.
	Notes []string
}

// Plan reads, for each of the stack's databases but those in skip, the
// owner connection under from and the one under to, each Live, Staged or
// Previous. A skipped database is left for the app's migrate to build.
func Plan(ctx context.Context, store dbroles.Store, f *fleet.Fleet, app, stack, from, to string, skip ...string) ([]Copy, error) {
	a, s, err := f.Stack(app, stack)
	if err != nil {
		return nil, fmt.Errorf("dbcopy: %w", err)
	}
	if from == to {
		return nil, errors.New("dbcopy: copying a database onto itself")
	}
	for _, sk := range skip {
		if !slices.ContainsFunc(s.Databases, func(d fleet.Database) bool { return d.Name == sk }) {
			return nil, fmt.Errorf("dbcopy: %s/%s has no database %s to skip", app, stack, sk)
		}
	}
	prefix := a.SecretPrefixFor(stack)
	var out []Copy
	for _, d := range s.Databases {
		if slices.Contains(skip, d.Name) {
			continue
		}
		c := Copy{Database: d.Name}
		if d.AppRole {
			c.AppRole = d.App()
		}
		for _, side := range []struct {
			dir string
			dst *string
		}{{from, &c.Source}, {to, &c.Target}} {
			v, note, err := ownerDSN(ctx, store, prefix, side.dir, d)
			if err != nil {
				return nil, err
			}
			if note != "" {
				c.Notes = append(c.Notes, note)
			}
			*side.dst = v
		}
		out = append(out, c)
	}
	return out, nil
}

// Owner is one database's owner connection, as Owners reads it.
type Owner struct {
	Database, DSN string
	// Note says how the connection was read; it holds no secret.
	Note string
}

// Owners reads the owner connection of each of the stack's databases under
// dir; with only, of just those databases.
func Owners(ctx context.Context, store dbroles.Store, f *fleet.Fleet, app, stack, dir string, only ...string) ([]Owner, error) {
	a, s, err := f.Stack(app, stack)
	if err != nil {
		return nil, fmt.Errorf("dbcopy: %w", err)
	}
	prefix := a.SecretPrefixFor(stack)
	var out []Owner
	for _, d := range s.Databases {
		if len(only) > 0 && !slices.Contains(only, d.Name) {
			continue
		}
		v, note, err := ownerDSN(ctx, store, prefix, dir, d)
		if err != nil {
			return nil, err
		}
		out = append(out, Owner{Database: d.Name, DSN: v, Note: note})
	}
	return out, nil
}

// ownerDSN reads d's owner connection under dir. It must reach d: a
// connection to another database would copy, or count, the wrong one. A
// migrate-only owner connection is a base connection, which migrate
// switches to its database itself, so it is switched here too, with a
// note.
func ownerDSN(ctx context.Context, store dbroles.Store, prefix, dir string, d fleet.Database) (dsn, note string, err error) {
	if d.OwnerDSN == "" {
		return "", "", fmt.Errorf("dbcopy: database %s names no ownerDsn parameter", d.Name)
	}
	name := prefix + dir + d.OwnerDSN
	v, found, err := store.Get(ctx, name)
	switch {
	case err != nil:
		return "", "", fmt.Errorf("dbcopy: reading %s: %w", name, err)
	case !found:
		return "", "", fmt.Errorf("dbcopy: %s does not exist", name)
	case strings.ContainsAny(v, "\n\r"):
		return "", "", fmt.Errorf("dbcopy: %s is not one line", name)
	}
	p, err := pgutil.ParseDSN(v)
	if err != nil {
		return "", "", fmt.Errorf("dbcopy: %s: %w", name, err)
	}
	if p.Database == d.Name {
		return v, "", nil
	}
	if !strings.HasPrefix(d.OwnerDSN, MigrateDir) {
		return "", "", fmt.Errorf("dbcopy: %s names database %q, not %s, as user %s on %s", name, p.Database, d.Name, p.User, p.Host)
	}
	if v, err = pgutil.WithDatabase(v, d.Name); err != nil {
		return "", "", fmt.Errorf("dbcopy: %s: %w", name, err)
	}
	return v, fmt.Sprintf("%s is a base connection to %q as %s; switched to %s", name, p.Database, p.User, d.Name), nil
}

// copyCommand reads the two connections from standard input, then runs
// copyScript, which follows them on standard input, with bash.
const copyCommand = `read -r SRC && read -r DST && read -r APP && export SRC DST APP && exec bash -s`

// copyScript dumps the source and restores it into the target, on the
// instance, in one transaction: a failure leaves the target as it was. The
// transaction first drops everything the target's owner has in it
// (clearTarget), so the target ends with exactly the source's objects, even
// when a newer release's migrate has made tables the source lacks. The
// restore takes no owners or grants from the source, so the target's owner
// owns everything.
//
// The dump is plain SQL piped to psql between BEGIN and a COMMIT sent only
// once pg_dump has succeeded: if the dump fails, psql reaches the end of its
// input with the transaction open, and it is rolled back.
//
// A table whose row-level security is forced binds its owner too, and
// pg_dump refuses it rather than dump only the rows a policy shows. So the
// source's FORCE is lifted on the tables the connecting role owns, which
// changes nothing for any other role, such as the app's app role, and is
// always put back; the target's same tables get FORCE after the restore.
const copyScript = `set -euo pipefail
problems=$(psql -X -q -A -t -v ON_ERROR_STOP=1 -v app="$APP" --dbname="$SRC" <<'GUARD'
` + guardQuery + `
GUARD
)
if [ -n "$problems" ]; then
  echo "db copy refuses this database: a restore with no owners or grants would drop or weaken each of these, so the app's own migrate must build it:" >&2
  echo "$problems" >&2
  exit 1
fi
forced=$(psql -X -q -A -t -v ON_ERROR_STOP=1 --dbname="$SRC" -c "SELECT c.oid::regclass FROM pg_catalog.pg_class c WHERE c.relkind = 'r' AND c.relforcerowsecurity AND pg_catalog.pg_get_userbyid(c.relowner) = current_user")
force() {
  [ -n "$forced" ] || return 0
  sql=""
  while IFS= read -r t; do sql="$sql ALTER TABLE $t $2 FORCE ROW LEVEL SECURITY;"; done <<< "$forced"
  psql -X -q -v ON_ERROR_STOP=1 --dbname="$1" -c "$sql"
}
trap 'force "$SRC" ""' EXIT
force "$SRC" NO
{
  echo 'BEGIN;'
  cat <<'CLEAR'
` + clearTarget + `
CLEAR
  pg_dump --format=plain --no-owner --no-privileges --dbname="$SRC" && echo 'COMMIT;'
} | psql -X -q -v ON_ERROR_STOP=1 --dbname="$DST" >/dev/null
force "$DST" ""
`

// clearTarget drops what the connecting role owns in the database: its own
// schemas but public, whole, then in public its views, tables, sequences,
// routines and types. An extension's objects stay; so does anything another
// role owns, which the restore then fails on or the counts report.
const clearTarget = `SET client_min_messages = warning;
DO $clear$
DECLARE r record;
BEGIN
  FOR r IN SELECT n.nspname FROM pg_catalog.pg_namespace n
    WHERE ` + schemaFilter + ` AND n.nspname <> 'public' AND n.nspowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
  LOOP EXECUTE format('DROP SCHEMA %I CASCADE', r.nspname); END LOOP;
  FOR r IN SELECT c.relkind, c.oid::regclass::text AS name FROM pg_catalog.pg_class c
    WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('v', 'm', 'r', 'p', 'f', 'S')
      AND pg_catalog.pg_get_userbyid(c.relowner) = current_user
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
    ORDER BY array_position(ARRAY['v', 'm', 'r', 'p', 'f', 'S']::"char"[], c.relkind)
  LOOP
    -- An earlier CASCADE may have dropped it already.
    IF to_regclass(r.name) IS NOT NULL THEN
      EXECUTE format('DROP %s %s CASCADE', CASE r.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW'
        WHEN 'f' THEN 'FOREIGN TABLE' WHEN 'S' THEN 'SEQUENCE' ELSE 'TABLE' END, r.name);
    END IF;
  END LOOP;
  FOR r IN SELECT p.oid::regprocedure::text AS name FROM pg_catalog.pg_proc p
    WHERE p.pronamespace = 'public'::regnamespace AND pg_catalog.pg_get_userbyid(p.proowner) = current_user
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
  LOOP EXECUTE format('DROP ROUTINE IF EXISTS %s CASCADE', r.name); END LOOP;
  FOR r IN SELECT t.typtype, t.oid::regtype::text AS name FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_class c ON c.oid = t.typrelid
    WHERE t.typnamespace = 'public'::regnamespace AND pg_catalog.pg_get_userbyid(t.typowner) = current_user
      AND (t.typtype IN ('e', 'd', 'r', 'm') OR (t.typtype = 'c' AND c.relkind = 'c'))
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
  LOOP
    IF to_regtype(r.name) IS NOT NULL THEN
      EXECUTE format('DROP %s %s CASCADE', CASE r.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END, r.name);
    END IF;
  END LOOP;
END
$clear$;`

// schemaFilter keeps a query to the database's own objects: not the
// system's, and not an extension's.
const schemaFilter = `n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'`

// guardQuery lists, one per line, what a restore with no owners and no
// grants would lose or loosen: objects owned by another role than the
// connecting one, policies that name a role, grants to any role but the
// database's app role (psql variable app), whose grants migrate gives
// again, and functions whose EXECUTE is not PUBLIC's. None means the copy
// keeps everything that matters.
const guardQuery = `WITH me AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
SELECT x FROM (
  SELECT format('%s is owned by %s', c.oid::regclass, pg_catalog.pg_get_userbyid(c.relowner)) AS x
  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND ` + schemaFilter + `
    AND c.relowner <> (SELECT oid FROM me)
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
  UNION ALL
  SELECT format('function %s is owned by %s', p.oid::regprocedure, pg_catalog.pg_get_userbyid(p.proowner))
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE ` + schemaFilter + ` AND p.proowner <> (SELECT oid FROM me)
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
  UNION ALL
  SELECT format('function %s does not let PUBLIC execute it', p.oid::regprocedure)
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE ` + schemaFilter + ` AND p.proacl IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.aclexplode(p.proacl) a WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
  UNION ALL
  SELECT format('policy %s on %s names role %s', pol.polname, pol.polrelid::regclass, pg_catalog.pg_get_userbyid(r))
  FROM pg_catalog.pg_policy pol, unnest(pol.polroles) AS r WHERE r <> 0
  UNION ALL
  SELECT format('%s grants %s to %s', c.oid::regclass, a.privilege_type,
    CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END)
  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace, pg_catalog.aclexplode(c.relacl) a
  WHERE c.relacl IS NOT NULL AND ` + schemaFilter + ` AND a.grantee <> c.relowner
    AND (a.grantee = 0 OR pg_catalog.pg_get_userbyid(a.grantee) <> :'app')
  UNION ALL
  SELECT format('function %s grants EXECUTE to %s', p.oid::regprocedure, pg_catalog.pg_get_userbyid(a.grantee))
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace, pg_catalog.aclexplode(p.proacl) a
  WHERE p.proacl IS NOT NULL AND ` + schemaFilter + ` AND a.grantee NOT IN (0, p.proowner)
    AND pg_catalog.pg_get_userbyid(a.grantee) <> :'app'
) s ORDER BY 1;`

// Remote runs a command on a fleet host: dbroles.Remote over ssh.
type Remote interface {
	Exec(ctx context.Context, command, stdin string) (string, error)
}

// Run copies each database in order, stopping at the first failure.
func Run(ctx context.Context, copies []Copy, r Remote) error {
	for _, c := range copies {
		if _, err := r.Exec(ctx, copyCommand, c.Source+"\n"+c.Target+"\n"+c.AppRole+"\n"+copyScript); err != nil {
			return fmt.Errorf("dbcopy: copying %s: %w", c.Database, err)
		}
	}
	return nil
}

// liftForce lifts FORCE on the row-level security of the connecting role's
// own tables, for the rest of the transaction.
const liftForce = `DO $dbcopy$ DECLARE t regclass; BEGIN ` +
	`FOR t IN SELECT c.oid::regclass FROM pg_catalog.pg_class c WHERE c.relkind = 'r' AND c.relforcerowsecurity ` +
	`AND pg_catalog.pg_get_userbyid(c.relowner) = current_user LOOP ` +
	`EXECUTE format('ALTER TABLE %s NO FORCE ROW LEVEL SECURITY', t); END LOOP; END $dbcopy$;`

// Unreadable is the count of a table the role cannot read.
const Unreadable = -1

// countQuery lists every table from the catalog, so a table the role has
// no privilege on is never left out: "schema.table count", or
// "schema.table unreadable", sorted. information_schema would hide it.
const countQuery = `SELECT n.nspname || '.' || c.relname || ' ' || ` +
	`CASE WHEN pg_catalog.has_table_privilege(c.oid, 'SELECT') ` +
	`THEN (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text ` +
	`ELSE 'unreadable' END ` +
	`FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
	`WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema') ` +
	`AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%' ORDER BY 1;`

// Counts returns each table's exact row count in the database dsn names,
// read on the instance; a table the role cannot read counts Unreadable.
func Counts(ctx context.Context, dsn string, r Remote) (map[string]int64, error) {
	// Counted in a transaction that lifts FORCE on the role's own tables and
	// is rolled back, so a forced policy does not hide rows from the count.
	script := "BEGIN;\n" + liftForce + "\n" + countQuery + "\nROLLBACK;\n"
	out, err := r.Exec(ctx, `read -r DSN && exec psql -X -q -A -t -v ON_ERROR_STOP=1 --dbname="$DSN"`, dsn+"\n"+script)
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		table, n, ok := strings.Cut(l, " ")
		if !ok {
			return nil, fmt.Errorf("dbcopy: unexpected count line %q", l)
		}
		if n == "unreadable" {
			counts[table] = Unreadable
			continue
		}
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("dbcopy: count of %s: %w", table, err)
		}
		counts[table] = v
	}
	return counts, nil
}

// CompareCounts returns each difference between the source's and the
// target's tables and row counts, sorted; none means they match.
func CompareCounts(src, dst map[string]int64) []string {
	var diffs []string
	for t, n := range src {
		m, ok := dst[t]
		switch {
		case n == Unreadable:
			diffs = append(diffs, fmt.Sprintf("%s: the source's role cannot read it, so it cannot be copied", t))
		case m == Unreadable:
			diffs = append(diffs, fmt.Sprintf("%s: the target's role cannot read it", t))
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s: %d rows in the source, missing from the target", t, n))
		case m != n:
			diffs = append(diffs, fmt.Sprintf("%s: %d rows in the source, %d in the target", t, n, m))
		}
	}
	for t, m := range dst {
		if _, ok := src[t]; !ok && m == Unreadable {
			diffs = append(diffs, fmt.Sprintf("%s: in the target, which its role cannot read, not in the source", t))
		} else if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: %d rows in the target, not in the source", t, m))
		}
	}
	slices.Sort(diffs)
	return diffs
}

// Promote moves a staged stack's parameters into place: each live
// parameter is kept under Previous, unless one is there already, and then
// replaced with its staged value. It returns the live names it replaced or
// created. Deploys then use the new server; the old one keeps running on
// what it was deployed with, and must not be deployed again.
func Promote(ctx context.Context, store Store, f *fleet.Fleet, app, stack string) ([]string, error) {
	a, s, err := f.Stack(app, stack)
	if err != nil {
		return nil, fmt.Errorf("dbcopy: %w", err)
	}
	prefix := a.SecretPrefixFor(stack)
	// Each database's parameters, as dbroles makes them: the owner's
	// connection, and the app role's when it has one.
	var names []string
	for _, d := range s.Databases {
		if d.OwnerDSN == "" {
			return nil, fmt.Errorf("dbcopy: database %s names no ownerDsn parameter", d.Name)
		}
		names = append(names, d.OwnerDSN)
		if d.AppRole {
			names = append(names, d.AppDSN)
			if d.AppPassword != "" {
				names = append(names, d.AppPassword)
			}
		}
	}
	// Every staged value must exist before anything changes.
	staged := map[string]string{}
	for _, n := range names {
		v, found, err := store.Get(ctx, prefix+Staged+n)
		switch {
		case err != nil:
			return nil, fmt.Errorf("dbcopy: reading %s: %w", prefix+Staged+n, err)
		case !found:
			return nil, fmt.Errorf("dbcopy: %s is not staged; run db roles apply first", prefix+Staged+n)
		}
		staged[n] = v
	}
	var done []string
	for _, n := range names {
		live := prefix + n
		old, found, err := store.Get(ctx, live)
		if err != nil {
			return done, fmt.Errorf("dbcopy: reading %s: %w", live, err)
		}
		if found && old == staged[n] {
			continue
		}
		if found {
			if _, kept, err := store.Get(ctx, prefix+Previous+n); err != nil {
				return done, fmt.Errorf("dbcopy: reading %s: %w", prefix+Previous+n, err)
			} else if !kept {
				if err := store.Put(ctx, prefix+Previous+n, old); err != nil {
					return done, fmt.Errorf("dbcopy: keeping %s: %w", live, err)
				}
			}
		}
		if err := store.Set(ctx, live, staged[n]); err != nil {
			return done, fmt.Errorf("dbcopy: writing %s: %w", live, err)
		}
		done = append(done, live)
	}
	return done, nil
}
