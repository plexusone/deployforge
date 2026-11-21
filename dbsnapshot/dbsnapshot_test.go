package dbsnapshot

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	"github.com/aws/aws-sdk-go-v2/service/lightsail/types"
)

// fake is a Lightsail account with one server. Each snapshot moves through
// states, one per GetRelationalDatabaseSnapshot call.
type fake struct {
	server, serverState string
	snapshots           []types.RelationalDatabaseSnapshot
	states              []string // the states a new snapshot reports, in turn
	created             []*lightsail.CreateRelationalDatabaseSnapshotInput
	polls               int
	pageSize            int
}

func (f *fake) GetRelationalDatabase(_ context.Context, in *lightsail.GetRelationalDatabaseInput, _ ...func(*lightsail.Options)) (*lightsail.GetRelationalDatabaseOutput, error) {
	if aws.ToString(in.RelationalDatabaseName) != f.server {
		return nil, errors.New("NotFoundException")
	}
	return &lightsail.GetRelationalDatabaseOutput{RelationalDatabase: &types.RelationalDatabase{Name: aws.String(f.server), State: aws.String(f.serverState)}}, nil
}

func (f *fake) CreateRelationalDatabaseSnapshot(_ context.Context, in *lightsail.CreateRelationalDatabaseSnapshotInput, _ ...func(*lightsail.Options)) (*lightsail.CreateRelationalDatabaseSnapshotOutput, error) {
	f.created = append(f.created, in)
	f.snapshots = append(f.snapshots, types.RelationalDatabaseSnapshot{
		Name: in.RelationalDatabaseSnapshotName, FromRelationalDatabaseName: in.RelationalDatabaseName,
		State: aws.String("pending"), Tags: in.Tags,
	})
	return &lightsail.CreateRelationalDatabaseSnapshotOutput{}, nil
}

func (f *fake) GetRelationalDatabaseSnapshot(_ context.Context, in *lightsail.GetRelationalDatabaseSnapshotInput, _ ...func(*lightsail.Options)) (*lightsail.GetRelationalDatabaseSnapshotOutput, error) {
	for i := range f.snapshots {
		s := &f.snapshots[i]
		if aws.ToString(s.Name) != aws.ToString(in.RelationalDatabaseSnapshotName) {
			continue
		}
		if f.polls < len(f.states) {
			s.State = aws.String(f.states[f.polls])
		}
		f.polls++
		return &lightsail.GetRelationalDatabaseSnapshotOutput{RelationalDatabaseSnapshot: s}, nil
	}
	return nil, errors.New("NotFoundException")
}

func (f *fake) GetRelationalDatabaseSnapshots(_ context.Context, in *lightsail.GetRelationalDatabaseSnapshotsInput, _ ...func(*lightsail.Options)) (*lightsail.GetRelationalDatabaseSnapshotsOutput, error) {
	size := f.pageSize
	if size == 0 {
		size = len(f.snapshots)
	}
	start := 0
	if in.PageToken != nil {
		n, err := strconv.Atoi(*in.PageToken)
		if err != nil {
			return nil, err
		}
		start = n
	}
	end := min(start+size, len(f.snapshots))
	out := &lightsail.GetRelationalDatabaseSnapshotsOutput{RelationalDatabaseSnapshots: f.snapshots[start:end]}
	if end < len(f.snapshots) {
		out.NextPageToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

var now = time.Date(2026, 11, 18, 19, 30, 5, 0, time.FixedZone("PST", -8*3600))

func noSleep(context.Context, time.Duration) error { return nil }

func TestName(t *testing.T) {
	for _, tc := range []struct {
		r    Request
		want string
	}{
		{Request{Server: "fleet-prod-pg", App: "one-cloud", Stack: "prod", Label: "v0.3.0", Now: now},
			"fleet-prod-pg-one-cloud-prod-v0-3-0-20261119-033005"},
		{Request{Server: "fleet-prod-pg", App: "two-cloud", Stack: "prod", Now: now},
			"fleet-prod-pg-two-cloud-prod-20261119-033005"},
		{Request{Server: "Fleet_Prod-PG", App: "a", Stack: "prod", Label: "local main@abc123!", Now: now},
			"fleet-prod-pg-a-prod-local-main-abc123-20261119-033005"},
	} {
		got, err := Name(tc.r)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Name(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
	long, err := Name(Request{Server: "s", App: "a", Stack: "prod", Label: strings.Repeat("x", 400), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > maxName || !strings.HasSuffix(long, "-20261119-033005") {
		t.Errorf("long name %d characters, %q", len(long), long[len(long)-20:])
	}
	if _, err := Name(Request{Server: "s", App: "", Stack: "prod", Now: now}); err == nil {
		t.Error("a request without an app is named")
	}
	if _, err := Name(Request{Server: "s", App: "a", Stack: "prod"}); err == nil {
		t.Error("a request without a time is named")
	}
}

func TestCreateWaitsUntilAvailable(t *testing.T) {
	f := &fake{server: "fleet-prod-pg", serverState: Available, states: []string{"pending", "pending", Available}}
	ctx := context.Background()
	p, err := Prepare(ctx, f, Request{Server: "fleet-prod-pg", App: "one-cloud", Stack: "prod", Label: "v0.3.0", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	s, err := Create(ctx, f, p, Wait{Sleep: noSleep}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != Available || s.Name != p.Name || f.polls != 3 {
		t.Errorf("snapshot %+v after %d polls", s, f.polls)
	}
	if len(f.created) != 1 || aws.ToString(f.created[0].RelationalDatabaseName) != "fleet-prod-pg" {
		t.Fatalf("created %+v", f.created)
	}
	if s.Tags["app"] != "one-cloud" || s.Tags["stack"] != "prod" || s.Tags["label"] != "v0.3.0" {
		t.Errorf("tags %v", s.Tags)
	}
	// One line when it starts, one per change of state.
	if n := strings.Count(log.String(), "\n"); n != 2 {
		t.Errorf("log has %d lines:\n%s", n, log.String())
	}
}

func TestCreateRefusesABusyServer(t *testing.T) {
	f := &fake{server: "fleet-prod-pg", serverState: "backing-up"}
	p, err := Prepare(context.Background(), f, Request{Server: "fleet-prod-pg", App: "a", Stack: "prod", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), f, p, Wait{Sleep: noSleep}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "backing-up") {
		t.Fatalf("err = %v", err)
	}
	if len(f.created) != 0 {
		t.Error("a snapshot was started on a busy server")
	}
}

func TestPrepareUnknownServer(t *testing.T) {
	f := &fake{server: "fleet-prod-pg", serverState: Available}
	if _, err := Prepare(context.Background(), f, Request{Server: "other-pg", App: "a", Stack: "prod", Now: now}); err == nil {
		t.Fatal("an unknown server is planned")
	}
}

func TestAwaitFailsAndTimesOut(t *testing.T) {
	ctx := context.Background()
	f := &fake{server: "s", snapshots: []types.RelationalDatabaseSnapshot{{Name: aws.String("x"), FromRelationalDatabaseName: aws.String("s")}},
		states: []string{"pending", "failed"}}
	if _, err := Await(ctx, f, "x", Wait{Sleep: noSleep}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a failed snapshot: err = %v", err)
	}

	f = &fake{server: "s", snapshots: []types.RelationalDatabaseSnapshot{{Name: aws.String("x"), FromRelationalDatabaseName: aws.String("s"), State: aws.String("pending")}}}
	expired := func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	if _, err := Await(ctx, f, "x", Wait{Sleep: expired}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "still pending") {
		t.Fatalf("a timed-out wait: err = %v", err)
	}
}

func TestListAndVerify(t *testing.T) {
	at := func(h int) *time.Time { t := time.Date(2026, 11, 18, h, 0, 0, 0, time.UTC); return &t }
	f := &fake{server: "s", pageSize: 1, snapshots: []types.RelationalDatabaseSnapshot{
		{Name: aws.String("old"), FromRelationalDatabaseName: aws.String("s"), State: aws.String(Available), CreatedAt: at(1)},
		{Name: aws.String("other"), FromRelationalDatabaseName: aws.String("t"), State: aws.String(Available), CreatedAt: at(2)},
		{Name: aws.String("new"), FromRelationalDatabaseName: aws.String("s"), State: aws.String("pending"), CreatedAt: at(3)},
	}}
	ctx := context.Background()
	got, err := List(ctx, f, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "new" || got[1].Name != "old" {
		t.Fatalf("List = %+v", got)
	}
	if _, err := Verify(ctx, f, "old"); err != nil {
		t.Errorf("Verify(old) = %v", err)
	}
	if _, err := Verify(ctx, f, "new"); err == nil {
		t.Error("a pending snapshot verifies")
	}
	if _, err := Verify(ctx, f, "missing"); err == nil {
		t.Error("a missing snapshot verifies")
	}
}
