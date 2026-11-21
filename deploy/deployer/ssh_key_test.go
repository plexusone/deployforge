package deployer

import (
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func TestSSHUsesTheGivenKeyThroughTheAgent(t *testing.T) {
	plain, err := NewSSH("ubuntu@203.0.113.7", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plain.options(), " "); got != "-o BatchMode=yes" {
		t.Errorf("no key: %s", got)
	}
	keyed, err := NewSSH("ubuntu@203.0.113.7", "/srv/keys/fleet")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keyed.options(), " "); got != "-o BatchMode=yes -i /srv/keys/fleet -o IdentityAgent=SSH_AUTH_SOCK" {
		t.Errorf("a key: %s", got)
	}
	if _, err := NewSSH("ubuntu@203.0.113.7", "-oProxyCommand=x"); err == nil {
		t.Error("a key path that is an option was accepted")
	}
}

func TestSSHDialerTargetsTheHostsUserAndAddress(t *testing.T) {
	tr, err := SSHDialer("")(fleet.Host{Name: "web-1", Address: "203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	if s := tr.(*SSH); s.Host != "ubuntu@203.0.113.10" {
		t.Errorf("default user: %s", s.Host)
	}
	tr, err = SSHDialer("")(fleet.Host{Name: "web-1", Address: "web1.example.com", SSHUser: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if s := tr.(*SSH); s.Host != "deploy@web1.example.com" {
		t.Errorf("named user: %s", s.Host)
	}
	if _, err := SSHDialer("")(fleet.Host{Name: "web-1", Address: "{{output:staticIp}}"}); err == nil {
		t.Error("an unresolved address was dialled")
	}
}
