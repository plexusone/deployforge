// Package dbsnapshot takes a manual snapshot of the fleet's managed
// PostgreSQL server (a Lightsail relational database) and waits until it is
// available, so a deploy whose migrate changes a schema can be undone by
// restoring the server as it was just before.
//
// The server may be shared, so a snapshot holds every app's databases; the
// app, stack and label name it and are kept as tags, to say which deploy it
// was taken for. The automatic daily backups and point-in-time restore stay
// as they are. Nothing here deletes a snapshot: the operator removes old ones
// in the Lightsail console.
package dbsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	"github.com/aws/aws-sdk-go-v2/service/lightsail/types"
)

// API is the part of the Lightsail client used here.
type API interface {
	GetRelationalDatabase(ctx context.Context, in *lightsail.GetRelationalDatabaseInput, opts ...func(*lightsail.Options)) (*lightsail.GetRelationalDatabaseOutput, error)
	CreateRelationalDatabaseSnapshot(ctx context.Context, in *lightsail.CreateRelationalDatabaseSnapshotInput, opts ...func(*lightsail.Options)) (*lightsail.CreateRelationalDatabaseSnapshotOutput, error)
	GetRelationalDatabaseSnapshot(ctx context.Context, in *lightsail.GetRelationalDatabaseSnapshotInput, opts ...func(*lightsail.Options)) (*lightsail.GetRelationalDatabaseSnapshotOutput, error)
	GetRelationalDatabaseSnapshots(ctx context.Context, in *lightsail.GetRelationalDatabaseSnapshotsInput, opts ...func(*lightsail.Options)) (*lightsail.GetRelationalDatabaseSnapshotsOutput, error)
}

// Available is the state of a server or snapshot that is ready.
const Available = "available"

// maxName is Lightsail's limit on a snapshot's name.
const maxName = 255

// Request says which server to snapshot and what the snapshot is for.
type Request struct {
	// Server is the Lightsail relational database's name.
	Server string
	// App and Stack are the deploy the snapshot is taken for.
	App, Stack string
	// Label is optional, such as the release tag being deployed.
	Label string
	// Now is when the snapshot is named; its UTC time is in the name.
	Now time.Time
}

// Snapshot is one manual snapshot of the server.
type Snapshot struct {
	Name, State, Server string
	Created             time.Time
	SizeGB              int32
	Tags                map[string]string
}

var notNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// slug lowercases s and turns each run of other characters into one hyphen,
// as Lightsail's names allow only letters, digits and hyphens.
func slug(s string) string {
	return strings.Trim(notNameChars.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// Name is the snapshot's name:
// <server>-<app>-<stack>[-<label>]-<yyyymmdd>-<hhmmss>, in UTC, such as
// fleet-prod-pg-one-cloud-prod-v0-3-0-20261118-193000.
func Name(r Request) (string, error) {
	if r.Now.IsZero() {
		return "", errors.New("dbsnapshot: no time to name the snapshot by")
	}
	parts := []string{slug(r.Server), slug(r.App), slug(r.Stack)}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("dbsnapshot: %q %q %q is not a server, app and stack", r.Server, r.App, r.Stack)
		}
	}
	if l := slug(r.Label); l != "" {
		parts = append(parts, l)
	}
	stamp := r.Now.UTC().Format("20060102-150405")
	name := strings.Join(parts, "-")
	if len(name)+1+len(stamp) > maxName {
		name = strings.TrimRight(name[:maxName-1-len(stamp)], "-")
	}
	return name + "-" + stamp, nil
}

// Plan is what Create would do.
type Plan struct {
	Request
	// Name is the snapshot Create would make.
	Name string
	// ServerState is the server's state now; Create needs it available.
	ServerState string
	// Recent are the server's manual snapshots, newest first.
	Recent []Snapshot
}

// Prepare checks that the server exists and names the snapshot. It changes
// nothing.
func Prepare(ctx context.Context, api API, r Request) (Plan, error) {
	name, err := Name(r)
	if err != nil {
		return Plan{}, err
	}
	out, err := api.GetRelationalDatabase(ctx, &lightsail.GetRelationalDatabaseInput{RelationalDatabaseName: aws.String(r.Server)})
	if err != nil {
		return Plan{}, fmt.Errorf("dbsnapshot: the server %s: %w", r.Server, err)
	}
	if out.RelationalDatabase == nil {
		return Plan{}, fmt.Errorf("dbsnapshot: no server %s", r.Server)
	}
	recent, err := List(ctx, api, r.Server)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Request: r, Name: name, ServerState: aws.ToString(out.RelationalDatabase.State), Recent: recent}, nil
}

// List returns the server's manual snapshots, newest first.
func List(ctx context.Context, api API, server string) ([]Snapshot, error) {
	var all []Snapshot
	var token *string
	for {
		out, err := api.GetRelationalDatabaseSnapshots(ctx, &lightsail.GetRelationalDatabaseSnapshotsInput{PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("dbsnapshot: listing snapshots: %w", err)
		}
		for _, s := range out.RelationalDatabaseSnapshots {
			if aws.ToString(s.FromRelationalDatabaseName) == server {
				all = append(all, fromAWS(s))
			}
		}
		token = out.NextPageToken
		if aws.ToString(token) == "" {
			break
		}
	}
	slices.SortFunc(all, func(a, b Snapshot) int { return b.Created.Compare(a.Created) })
	return all, nil
}

func fromAWS(s types.RelationalDatabaseSnapshot) Snapshot {
	tags := map[string]string{}
	for _, t := range s.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return Snapshot{
		Name:    aws.ToString(s.Name),
		State:   aws.ToString(s.State),
		Server:  aws.ToString(s.FromRelationalDatabaseName),
		Created: aws.ToTime(s.CreatedAt),
		SizeGB:  aws.ToInt32(s.SizeInGb),
		Tags:    tags,
	}
}

// Wait is how Create and Await poll for the snapshot.
type Wait struct {
	// Interval between polls; default 15 seconds.
	Interval time.Duration
	// Timeout for the snapshot to become available; default 30 minutes.
	Timeout time.Duration
	// Sleep waits d or until ctx is done; default a timer. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (w Wait) withDefaults() Wait {
	if w.Interval <= 0 {
		w.Interval = 15 * time.Second
	}
	if w.Timeout <= 0 {
		w.Timeout = 30 * time.Minute
	}
	if w.Sleep == nil {
		w.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return w
}

// Create makes the planned snapshot, tagged with the app, stack and label,
// and waits until it is available. The server must be available: Lightsail
// refuses a snapshot while it is backing up or being modified.
func Create(ctx context.Context, api API, p Plan, w Wait, log io.Writer) (Snapshot, error) {
	if p.ServerState != Available {
		return Snapshot{}, fmt.Errorf("dbsnapshot: the server %s is %q, not %s; try again shortly", p.Server, p.ServerState, Available)
	}
	tags := []types.Tag{{Key: aws.String("app"), Value: aws.String(p.App)}, {Key: aws.String("stack"), Value: aws.String(p.Stack)}}
	if p.Label != "" {
		tags = append(tags, types.Tag{Key: aws.String("label"), Value: aws.String(p.Label)})
	}
	if _, err := api.CreateRelationalDatabaseSnapshot(ctx, &lightsail.CreateRelationalDatabaseSnapshotInput{
		RelationalDatabaseName:         aws.String(p.Server),
		RelationalDatabaseSnapshotName: aws.String(p.Name),
		Tags:                           tags,
	}); err != nil {
		return Snapshot{}, fmt.Errorf("dbsnapshot: creating %s: %w", p.Name, err)
	}
	if _, err := fmt.Fprintf(log, "snapshot %s of %s started; waiting until it is available\n", p.Name, p.Server); err != nil {
		return Snapshot{}, err
	}
	return Await(ctx, api, p.Name, w, log)
}

// Await polls the snapshot until it is available, fails, or the wait times
// out.
func Await(ctx context.Context, api API, name string, w Wait, log io.Writer) (Snapshot, error) {
	w = w.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	last := ""
	for {
		s, err := Get(ctx, api, name)
		if err != nil {
			return Snapshot{}, err
		}
		switch s.State {
		case Available:
			return s, nil
		case "failed", "error":
			return s, fmt.Errorf("dbsnapshot: %s is %s", name, s.State)
		}
		if s.State != last {
			if _, err := fmt.Fprintf(log, "  %s is %s\n", name, s.State); err != nil {
				return Snapshot{}, err
			}
			last = s.State
		}
		if err := w.Sleep(ctx, w.Interval); err != nil {
			return s, fmt.Errorf("dbsnapshot: %s is still %s after %s: %w", name, s.State, w.Timeout, err)
		}
	}
}

// Get returns one snapshot.
func Get(ctx context.Context, api API, name string) (Snapshot, error) {
	out, err := api.GetRelationalDatabaseSnapshot(ctx, &lightsail.GetRelationalDatabaseSnapshotInput{RelationalDatabaseSnapshotName: aws.String(name)})
	if err != nil {
		return Snapshot{}, fmt.Errorf("dbsnapshot: %s: %w", name, err)
	}
	if out.RelationalDatabaseSnapshot == nil {
		return Snapshot{}, fmt.Errorf("dbsnapshot: no snapshot %s", name)
	}
	return fromAWS(*out.RelationalDatabaseSnapshot), nil
}

// Verify returns an error unless the snapshot is available.
func Verify(ctx context.Context, api API, name string) (Snapshot, error) {
	s, err := Get(ctx, api, name)
	if err != nil {
		return Snapshot{}, err
	}
	if s.State != Available {
		return s, fmt.Errorf("dbsnapshot: %s is %s, not %s", name, s.State, Available)
	}
	return s, nil
}
