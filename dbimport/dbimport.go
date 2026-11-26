// Package dbimport loads a data-only dump into one of a stack's databases,
// as its owner, on the database server's via host: the data a local copy
// of an app holds, carried into a stack that already has its schema.
//
// The load is one transaction. Without apply it is rolled back, so a dry
// run shows each table's row count as the load would leave it and changes
// nothing. Tables whose row-level security is forced on their owner have
// FORCE lifted for the load and put back in the same transaction. The
// dump may hold only SQL and COPY data: any psql meta-command outside COPY
// data is refused, apart from pg_dump's \restrict and \unrestrict lines,
// which are dropped. The owner's connection travels on ssh's standard
// input, never as an argument.
package dbimport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Remote runs a fixed command on a fleet host with stdin as its standard
// input: dbroles.Remote over ssh.
type Remote interface {
	Exec(ctx context.Context, command, stdin string) (string, error)
}

// Command is the remote command: psql reading the connection's line, then
// its script from standard input, printing rows bare.
const Command = `read -r DSN && exec psql -X -q -A -t -v ON_ERROR_STOP=1 --dbname="$DSN" -f - 2>&1`

var (
	copyTable = regexp.MustCompile(`(?m)^COPY public\.([a-z_][a-z0-9_]*) \(`)
	tableName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// Tables are the public tables a dump's COPY statements fill, in order.
func Tables(data []byte) []string {
	var out []string
	for _, m := range copyTable.FindAllSubmatch(data, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// Replacement is text to replace throughout the dump before it loads,
// such as an ID that differs between the source and the stack, or a local
// host name.
type Replacement struct {
	Old, New string
}

// ParseReplacement reads OLD=NEW.
func ParseReplacement(s string) (Replacement, error) {
	oldText, newText, ok := strings.Cut(s, "=")
	if !ok || oldText == "" {
		return Replacement{}, fmt.Errorf("dbimport: %q is not OLD=NEW", s)
	}
	return Replacement{Old: oldText, New: newText}, nil
}

// Replace applies the replacements to the dump, all at once, so one's new
// text is never replaced again by another.
func Replace(data []byte, rs []Replacement) []byte {
	if len(rs) == 0 {
		return data
	}
	pairs := make([]string, 0, 2*len(rs))
	for _, r := range rs {
		pairs = append(pairs, r.Old, r.New)
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(data)))
}

// Import loads data, a plain-SQL data-only dump (pg_dump --data-only), into
// the database dsn connects to as its owner, and returns psql's output:
// each table's row count after the load, while the transaction is still
// open. On any error, such as a row whose key the database already has,
// nothing is loaded. Without apply the transaction is rolled back.
func Import(ctx context.Context, dsn string, data []byte, apply bool, r Remote) (string, error) {
	if strings.ContainsAny(dsn, "\n\r") {
		return "", errors.New("dbimport: the connection must be one line")
	}
	tables := Tables(data)
	if len(tables) == 0 {
		return "", errors.New("dbimport: the dump has no COPY data for public tables")
	}
	for _, t := range tables {
		if !tableName.MatchString(t) {
			return "", fmt.Errorf("dbimport: %q is not a table name", t)
		}
	}
	body, err := StripMetaCommands(data)
	if err != nil {
		return "", err
	}
	out, err := r.Exec(ctx, Command, dsn+"\n"+Script(body, tables, apply))
	if err != nil {
		return out, fmt.Errorf("dbimport: importing: %w: %s", err, strings.TrimSpace(out))
	}
	return out, nil
}

// Script is the transaction Import runs: FORCE lifted, the dump, each
// table's count with row security off, FORCE put back, then COMMIT with
// apply or ROLLBACK without.
func Script(body string, tables []string, apply bool) string {
	end := "ROLLBACK;"
	if apply {
		end = "COMMIT;"
	}
	var b strings.Builder
	b.WriteString("BEGIN;\n" + forceAll("NO FORCE") + "\n")
	b.WriteString(body)
	// Counted while FORCE is lifted, so the owner sees every row.
	b.WriteString("\nSET row_security = off;\n")
	for _, t := range tables {
		fmt.Fprintf(&b, "SELECT %s || ': ' || count(*) || ' rows' FROM public.%s;\n", quoteLiteral(t), t)
	}
	b.WriteString(forceAll("FORCE") + "\n" + end + "\n")
	return b.String()
}

// forceAll sets FORCE or NO FORCE row-level security on every public table
// the connecting role owns that has row-level security enabled.
func forceAll(mode string) string {
	return `DO $dbimport$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass::text AS t FROM pg_catalog.pg_class c
    WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' AND c.relrowsecurity
      AND pg_catalog.pg_get_userbyid(c.relowner) = current_user
  LOOP EXECUTE format('ALTER TABLE %s ` + mode + ` ROW LEVEL SECURITY', r.t); END LOOP;
END
$dbimport$;`
}

// StripMetaCommands drops pg_dump's \restrict and \unrestrict lines, which
// a psql older than the dump's would refuse, and refuses any other psql
// meta-command outside COPY data: the dump must be data only.
func StripMetaCommands(data []byte) (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	inCopy := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case inCopy:
			if line == `\.` {
				inCopy = false
			}
		case strings.HasPrefix(line, "COPY ") && strings.HasSuffix(line, "FROM stdin;"):
			inCopy = true
		case strings.HasPrefix(line, `\restrict `), strings.HasPrefix(line, `\unrestrict `):
			continue
		case strings.HasPrefix(line, `\`):
			return "", fmt.Errorf("dbimport: the dump has a psql command, %.40q: import a data-only dump", line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("dbimport: reading the dump: %w", err)
	}
	if inCopy {
		return "", errors.New("dbimport: the dump ends inside COPY data")
	}
	return b.String(), nil
}

// quoteLiteral quotes s as a SQL string literal.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
