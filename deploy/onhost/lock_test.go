package onhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (r *rig) audit() []AuditEntry {
	r.t.Helper()
	f, err := os.Open(filepath.Join(r.root, "opt", "one-cloud", auditName))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			r.t.Error(err)
		}
	}()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func lockPath(r *rig) string { return filepath.Join(r.root, "opt", "one-cloud", lockName) }

func TestTheLockIsHeldOnlyWhileARequestRuns(t *testing.T) {
	r := newRig(t)
	if _, err := r.release(idA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath(r)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock stayed after a release: %v", err)
	}
	r.sys.fail["migrate"] = true
	if _, err := r.release(idB); err == nil {
		t.Fatal("a failing migrate succeeded")
	}
	if _, err := os.Stat(lockPath(r)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock stayed after a failure: %v", err)
	}
}

func TestAHeldLockRefusesAnotherDeployAndUnlockClearsIt(t *testing.T) {
	r := newRig(t)
	held := `{"operator":"alice","action":"release","release":"` + idA + `","since":"2026-10-23T18:00:00Z","pid":42}` + "\n"
	if err := os.WriteFile(lockPath(r), []byte(held), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.release(idB)
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Holder.Operator != "alice" || !strings.Contains(err.Error(), "unlock the stack") {
		t.Fatalf("err = %v", err)
	}
	if len(r.sys.calls) != 0 {
		t.Errorf("a locked stack was changed: %q", r.sys.calls)
	}
	if data, err := os.ReadFile(lockPath(r)); err != nil || string(data) != held {
		t.Errorf("another's lock was changed: %q, %v", data, err)
	}
	unlock := Request{Action: ActionUnlock, Target: tgt, Operator: "tester"}
	if _, err := r.host.Run(context.Background(), unlock, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.release(idB); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	// Unlocking a stack with no lock is not an error.
	if _, err := r.host.Run(context.Background(), unlock, ""); err != nil {
		t.Errorf("unlock without a lock: %v", err)
	}
}

func TestEveryRequestIsAuditedWithSecretsMasked(t *testing.T) {
	r := newRig(t)
	if _, err := r.release(idA); err != nil {
		t.Fatal(err)
	}
	// A failure whose message quotes a secret value.
	r.sys.fail["migrate"] = true
	r.sys.failMessage = "migrate printed SESSION_KEY=s3cr3t"
	if _, err := r.release(idB); err == nil {
		t.Fatal("a failing migrate succeeded")
	}
	got := r.audit()
	if len(got) != 2 {
		t.Fatalf("audit = %+v", got)
	}
	if a := got[0]; a.Outcome != "ok" || a.Action != ActionRelease || a.Release != idA || a.Operator != "tester" || a.Time.IsZero() {
		t.Errorf("first = %+v", a)
	}
	b := got[1]
	if b.Outcome != "failed" || b.Release != idB || b.Error == "" {
		t.Errorf("second = %+v", b)
	}
	if strings.Contains(b.Error, "s3cr3t") {
		t.Errorf("a secret reached the audit log: %q", b.Error)
	}
}
