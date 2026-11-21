package dbroles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/grokify/pgutil"
)

// hostPattern is a host name or address with nothing a shell would read.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

// PSQL runs SQL with psql on a fleet host over ssh, as the server's
// administrator. The password and the SQL travel on ssh's standard input,
// never as arguments, so no process listing shows them.
type PSQL struct {
	// SSH is ssh and its arguments, up to and including the destination,
	// such as ssh -i key -o BatchMode=yes ubuntu@203.0.113.10.
	SSH []string
	// Host and Port are the server's endpoint; Port is 5432 when zero.
	Host string
	Port int
	// User and Database are the administrator and the database it
	// connects to.
	User, Database string
	// Password is the administrator's password.
	Password string
}

// Run implements Runner. psql stops at the first error and prints rows
// unaligned, without headers.
func (p PSQL) Run(ctx context.Context, sql string) (string, error) {
	for _, v := range []string{p.User, p.Database} {
		if err := pgutil.ValidIdent(v); err != nil {
			return "", fmt.Errorf("dbroles: %w", err)
		}
	}
	if !hostPattern.MatchString(p.Host) {
		return "", fmt.Errorf("dbroles: host %q is not a host name", p.Host)
	}
	if strings.ContainsAny(p.Password, "\n\r") {
		return "", errors.New("dbroles: the administrator's password must be one line")
	}
	port := p.Port
	if port == 0 {
		port = 5432
	}
	conninfo := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=require", p.Host, port, p.User, p.Database)
	// The remote shell reads the password's line itself, then psql reads
	// the rest of standard input as its script.
	remote := `read -r PGPASSWORD && export PGPASSWORD && exec psql -X -q -A -t -v ON_ERROR_STOP=1 '` + conninfo + `'`
	return Remote{SSH: p.SSH}.Exec(ctx, remote, p.Password+"\n"+sql)
}

// Remote runs a fixed command on a host over ssh. Anything secret goes on
// standard input, never in the command.
type Remote struct {
	// SSH is ssh and its arguments, up to and including the destination.
	SSH []string
}

// Exec runs command on the host with stdin as its standard input and
// returns its standard output; the error carries its standard error.
func (r Remote) Exec(ctx context.Context, command, stdin string) (string, error) {
	if len(r.SSH) == 0 {
		return "", errors.New("dbroles: no ssh command")
	}
	args := append(append([]string{}, r.SSH[1:]...), command)
	cmd := exec.CommandContext(ctx, r.SSH[0], args...) //nolint:gosec // G204: ssh with the operator's own key and a fixed remote command
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
