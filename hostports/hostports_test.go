package hostports_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	"github.com/aws/aws-sdk-go-v2/service/lightsail/types"

	"github.com/plexusone/deployforge/hostports"
)

type fake struct {
	states []types.InstancePortState
	err    error
	asked  string
}

func (f *fake) GetInstancePortStates(_ context.Context, in *lightsail.GetInstancePortStatesInput, _ ...func(*lightsail.Options)) (*lightsail.GetInstancePortStatesOutput, error) {
	f.asked = *in.InstanceName
	return &lightsail.GetInstancePortStatesOutput{PortStates: f.states}, f.err
}

func port(n int32, cidrs ...string) types.InstancePortState {
	return types.InstancePortState{FromPort: n, ToPort: n, Protocol: types.NetworkProtocolTcp, State: types.PortStateOpen, Cidrs: cidrs}
}

var want = hostports.Want{
	Edge:  []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24")},
	Admin: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")},
}

func TestCheckPasses(t *testing.T) {
	f := &fake{states: []types.InstancePortState{port(443, "203.0.113.0/24", "198.51.100.0/24"), port(22, "192.0.2.10/32")}}
	r, err := hostports.Check(context.Background(), f, "web-1", want)
	if err != nil || f.asked != "web-1" {
		t.Fatalf("Check: %v, asked %q", err, f.asked)
	}
	if len(r.Problems) != 0 || len(r.OK) != 2 {
		t.Errorf("result %+v", r)
	}
}

func TestCheckFindsAClosedHTTPSPort(t *testing.T) {
	// The state a replace that runs new-first then deletes the old leaves.
	r := hostports.Compare([]types.InstancePortState{port(22, "192.0.2.10/32")}, want)
	if len(r.Problems) != 1 || r.Problems[0] != "tcp 443 is closed" {
		t.Errorf("problems %v", r.Problems)
	}
}

func TestCheckFindsWrongSourcesAndExtraPorts(t *testing.T) {
	r := hostports.Compare([]types.InstancePortState{
		port(443, "198.51.100.0/24", "0.0.0.0/0"),
		port(22, "192.0.2.10/32", "192.0.2.99/32"),
		port(80, "0.0.0.0/0"),
		{FromPort: 3389, ToPort: 3389, Protocol: types.NetworkProtocolTcp, State: types.PortStateClosed},
	}, want)
	got := strings.Join(r.Problems, "\n")
	for _, w := range []string{
		"tcp 443 is not open to 203.0.113.0/24",
		"tcp 443 is also open to 0.0.0.0/0",
		"tcp 22 is also open to 192.0.2.99/32",
		"tcp 80 is open to 0.0.0.0/0; only 443 and 22 should be",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("problems lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "3389") {
		t.Error("a closed port was reported")
	}
}

func TestCheckReportsTheAPIError(t *testing.T) {
	if _, err := hostports.Check(context.Background(), &fake{err: errors.New("denied")}, "web-1", want); err == nil {
		t.Error("no error")
	}
}
