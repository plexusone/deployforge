package onhost

import (
	"errors"
	"fmt"
)

// The steps a release or rollback can fail at, in the order they run.
const (
	StepUnpack        = "unpack"
	StepInstall       = "install"
	StepValidate      = "validate"
	StepSelfcheckPre  = "selfcheck-pre"
	StepMigrate       = "migrate"
	StepSwitch        = "switch"
	StepHealth        = "health"
	StepSelfcheckPost = "selfcheck-post"
)

// Failure is how a release or rollback failed: the step, and what it left.
// The host reports it with a failed request, so the operator's side can tell
// a deploy that changed nothing from one whose migration stays applied.
type Failure struct {
	// Step is the step that failed, such as StepMigrate.
	Step string `json:"step"`
	// Kept is true when the running release was never switched away from.
	Kept bool `json:"kept"`
	// Restored is true when a failed switch was undone: the previous
	// release, and its files, run again and passed the health check if
	// it was a rollback's restore.
	Restored bool `json:"restored"`
	// Stopped is true when a first release failed after the switch, so
	// nothing runs.
	Stopped bool `json:"stopped,omitempty"`
	// Migrated is true when the release's migration ran to completion:
	// a rollback or restore does not reverse it, so the running release
	// must work with the migrated schema.
	Migrated bool `json:"migrated"`
	// Error says what went wrong, with no secret value.
	Error string `json:"error"`
}

// StepError is a failed request with its Failure.
type StepError struct {
	Failure Failure
	Err     error
}

func (e *StepError) Error() string { return e.Err.Error() }
func (e *StepError) Unwrap() error { return e.Err }

// FailureOf returns err's Failure, when it is a StepError.
func FailureOf(err error) (Failure, bool) {
	var se *StepError
	if errors.As(err, &se) {
		return se.Failure, true
	}
	return Failure{}, false
}

// failed wraps err as the failure of step.
func failed(step string, f Failure, err error) error {
	f.Step = step
	f.Error = err.Error()
	return &StepError{Failure: f, Err: fmt.Errorf("%s: %w", step, err)}
}
