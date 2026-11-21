package onhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/plexusone/deployforge/deploy/secrets"
)

// The lock and the audit log live in the stack's /opt/<unit> directory.
const (
	lockName  = "deploy.lock"
	auditName = "audit.jsonl"
)

// Holder is who holds a stack's deploy lock, and for what.
type Holder struct {
	Operator string    `json:"operator"`
	Action   string    `json:"action"`
	Release  string    `json:"release,omitempty"`
	Since    time.Time `json:"since"`
	PID      int       `json:"pid"`
}

// LockedError means another deploy holds the stack's lock.
type LockedError struct {
	Holder Holder
}

func (e *LockedError) Error() string {
	h := e.Holder
	return fmt.Sprintf("onhost: a %s by %s has held the deploy lock since %s (pid %d); if no deploy is running, unlock the stack",
		h.Action, h.Operator, h.Since.UTC().Format(time.RFC3339), h.PID)
}

// lock takes the stack's deploy lock, so two deploys cannot overlap, and
// returns the function that releases it.
func (h *Host) lock(l layout, r Request) (func() error, error) {
	path := filepath.Join(l.base, lockName)
	body, err := json.Marshal(Holder{Operator: r.Operator, Action: r.Action, Release: r.ReleaseID, Since: h.now(), PID: os.Getpid()})
	if err != nil {
		return nil, fmt.Errorf("onhost: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a fixed name under the stack's directory
	if errors.Is(err, fs.ErrExist) {
		return nil, h.held(path)
	}
	if err != nil {
		return nil, fmt.Errorf("onhost: taking the deploy lock: %w", err)
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		return nil, errors.Join(fmt.Errorf("onhost: writing the deploy lock: %w", err), f.Close(), os.Remove(path))
	}
	if err := f.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("onhost: writing the deploy lock: %w", err), os.Remove(path))
	}
	return func() error {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("onhost: releasing the deploy lock: %w", err)
		}
		return nil
	}, nil
}

// held reports who holds the lock at path.
func (h *Host) held(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the lock this package wrote
	if err != nil {
		return fmt.Errorf("onhost: the stack is locked, and its lock cannot be read: %w", err)
	}
	var holder Holder
	if err := json.Unmarshal(data, &holder); err != nil {
		return fmt.Errorf("onhost: the stack is locked by an unreadable lock %s: %w", path, err)
	}
	return &LockedError{Holder: holder}
}

// unlock removes a stale lock. No lock is not an error.
func (h *Host) unlock(l layout) error {
	err := os.Remove(filepath.Join(l.base, lockName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("onhost: removing the deploy lock: %w", err)
	}
	return nil
}

// AuditEntry is one line of a stack's audit log on the host.
type AuditEntry struct {
	Time     time.Time `json:"time"`
	Operator string    `json:"operator"`
	Action   string    `json:"action"`
	// Release is the release the action asked for or made live.
	Release  string `json:"release,omitempty"`
	Previous string `json:"previous,omitempty"`
	// Outcome is ok or failed.
	Outcome string `json:"outcome"`
	// Error is why it failed, with every secret value masked.
	Error string `json:"error,omitempty"`
}

// audit appends one line for a request's outcome. It masks the request's
// secret values in the error, since an error can quote a command's output.
func (h *Host) audit(l layout, r Request, res Result, runErr error) error {
	e := AuditEntry{Time: h.now(), Operator: r.Operator, Action: r.Action, Release: r.ReleaseID, Outcome: "ok"}
	if res.Release != "" {
		e.Release, e.Previous = res.Release, res.Previous
	}
	if runErr != nil {
		e.Outcome = "failed"
		e.Error = secrets.NewRedactor(r.Secrets).String(runErr.Error())
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("onhost: %w", err)
	}
	path := filepath.Join(l.base, auditName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640) //nolint:gosec // G304: a fixed name under the stack's directory
	if err != nil {
		return fmt.Errorf("onhost: opening the audit log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return errors.Join(fmt.Errorf("onhost: writing the audit log: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("onhost: writing the audit log: %w", err)
	}
	return nil
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}
