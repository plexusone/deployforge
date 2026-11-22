package onhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ActionStatus reports a stack's releases and lock and changes nothing.
const ActionStatus = "status"

// Status is what is on the host for one stack.
type Status struct {
	// Live is the release current points at; empty when none does.
	Live string `json:"live,omitempty"`
	// Releases are the release directories kept, oldest first.
	Releases []string `json:"releases,omitempty"`
	// Lock is who holds the deploy lock, when it is held.
	Lock *Holder `json:"lock,omitempty"`
	// Audited says the stack has an audit log, which deployforge keeps.
	Audited bool `json:"audited"`
}

// status reads the stack's layout. A stack never deployed has no
// releases directory, which is not an error.
func (h *Host) status(l layout) (*Status, error) {
	var st Status
	live, err := liveRelease(l.current)
	if err != nil {
		return nil, err
	}
	if live != "" {
		st.Live = filepath.Base(live)
	}
	if _, err := os.Stat(l.releases); err == nil {
		if st.Releases, err = releaseNames(l.releases); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("onhost: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(l.base, lockName)) //nolint:gosec // G304: a fixed name under the stack's directory
	switch {
	case err == nil:
		var holder Holder
		if err := json.Unmarshal(data, &holder); err != nil {
			return nil, fmt.Errorf("onhost: the deploy lock is unreadable: %w", err)
		}
		st.Lock = &holder
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("onhost: reading the deploy lock: %w", err)
	}
	if _, err := os.Stat(filepath.Join(l.base, auditName)); err == nil {
		st.Audited = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("onhost: %w", err)
	}
	return &st, nil
}
