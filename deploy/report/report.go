// Package report is the outcome of a deploy command as one JSON document,
// for an agent or a script to read instead of the command's prose: whether
// it succeeded, the ledger entry it recorded, and when it failed, where and
// what it left running.
package report

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/plexusone/deployforge/deploy/onhost"
	"github.com/plexusone/deployforge/ledger"
)

// The phases of a ship, in the order they run. A release or rollback has
// only PhaseDeploy.
const (
	PhasePreflight = "preflight"
	PhasePackage   = "package"
	PhaseSnapshot  = "snapshot"
	PhaseDeploy    = "deploy"
	PhasePublic    = "public-check"
	PhaseStatus    = "status"
)

// Report is one command's outcome.
type Report struct {
	// Command is the command that ran: ship, release, rollback or status.
	Command string `json:"command"`
	App     string `json:"app"`
	Stack   string `json:"stack"`
	// OK is true when the command did all it set out to.
	OK bool `json:"ok"`
	// Phase is where the command failed; empty when it succeeded.
	Phase string `json:"phase,omitempty"`
	// Error says what went wrong; empty when it succeeded.
	Error string `json:"error,omitempty"`
	// Failure is how the host's part failed, when it got that far: the
	// step, and whether the running release was kept or restored.
	Failure *onhost.Failure `json:"failure,omitempty"`
	// Entry is the ledger entry the command recorded. A ship whose public
	// check failed has one: the release is live and recorded.
	Entry *ledger.Entry `json:"entry,omitempty"`
	// Snapshot is the database snapshot a ship took before deploying.
	Snapshot string `json:"snapshot,omitempty"`
	// Status is what a status command found on the host.
	Status *onhost.Status `json:"status,omitempty"`
	// Ledger is the ledger's current release, for a status command.
	Ledger string `json:"ledger,omitempty"`
}

// Fail records err as the failure of phase, with the host's Failure when
// err carries one.
func (r *Report) Fail(phase string, err error) {
	r.OK, r.Phase, r.Error = false, phase, err.Error()
	if f, ok := onhost.FailureOf(err); ok {
		r.Failure = &f
	}
}

// Write writes the report as one line of JSON.
func (r *Report) Write(w io.Writer) error {
	if r.OK && r.Phase != "" {
		return errors.New("report: a succeeded report names a failed phase")
	}
	return json.NewEncoder(w).Encode(r)
}
