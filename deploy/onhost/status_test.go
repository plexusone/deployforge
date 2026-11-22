package onhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusReadsTheStackAndChangesNothing(t *testing.T) {
	r := newRig(t)
	st := func() *Status {
		t.Helper()
		res, err := r.host.Run(context.Background(), Request{Action: ActionStatus, Target: tgt, Operator: "tester"}, "")
		if err != nil {
			t.Fatal(err)
		}
		return res.Status
	}
	if s := st(); s.Live != "" || len(s.Releases) != 0 || s.Lock != nil || s.Audited {
		t.Fatalf("a stack never deployed: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(r.root, "opt/one-cloud", auditName)); err == nil {
		t.Error("a status was audited")
	}
	if _, err := r.release(idA); err != nil {
		t.Fatal(err)
	}
	if _, err := r.release(idB); err != nil {
		t.Fatal(err)
	}
	s := st()
	if s.Live != idB || len(s.Releases) != 2 || s.Releases[0] != idA || !s.Audited || s.Lock != nil {
		t.Errorf("after two releases: %+v", s)
	}
	lock := filepath.Join(r.root, "opt/one-cloud", lockName)
	if err := os.WriteFile(lock, []byte(`{"operator":"alice","action":"release"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := st(); s.Lock == nil || s.Lock.Operator != "alice" {
		t.Errorf("a held lock: %+v", s.Lock)
	}
}
