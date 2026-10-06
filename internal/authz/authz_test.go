package authz

import (
	"testing"

	"github.com/quinnovator/inhouse/internal/spec"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const capName = "example.com/cap/inhouse"

func whois(tags []string, grants ...string) *apitype.WhoIsResponse {
	raw := []tailcfg.RawMessage{}
	for _, g := range grants {
		raw = append(raw, tailcfg.RawMessage(g))
	}
	return &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{StableID: "nSTABLE", Tags: tags},
		UserProfile: &tailcfg.UserProfile{LoginName: "alice@example.com"},
		CapMap:      tailcfg.PeerCapMap{capName: raw},
	}
}

func TestPrincipalIdentity(t *testing.T) {
	p, err := FromWhoIs(whois(nil, `{"role":"admin"}`), capName)
	if err != nil || p.ID != "alice@example.com" || !p.IsAdmin() {
		t.Fatal(p, err)
	}
	p, err = FromWhoIs(whois([]string{"tag:inhouse-agent"}, `{"role":"viewer","services":["*"]}`), capName)
	if err != nil || p.ID != "node:nSTABLE" || p.Login != "" {
		t.Fatal("tagged node must be identified by node, not by its creator", p, err)
	}
	if _, err = FromWhoIs(whois(nil), capName); err == nil {
		t.Fatal("no grants accepted")
	}
}

func TestMalformedGrantsAreDroppedWhole(t *testing.T) {
	for _, g := range []string{
		`{"role":"admin","services":["x"]}`,
		`{"role":"viewer"}`,
		`{"role":"deployer","services":["a/b"]}`,
		`{"role":"deployer","services":["["]}`,
		`{"role":"deployer","services":["x"],"expose":["public"]}`,
		`{"role":"deployer","services":["x"],"max_ttl":"forever"}`,
		`{"role":"deployer","services":["x"],"extra":true}`,
		`{"role":"owner"}`,
		`{"role":"admin"} {"role":"admin"}`,
	} {
		if _, err := FromWhoIs(whois(nil, g), capName); err == nil {
			t.Errorf("accepted %s", g)
		}
	}
}

func stack(name, expose string, ttl string) spec.Stack {
	s := spec.Stack{Name: name, Expose: expose}
	if ttl != "" {
		s.TTL = &ttl
	}
	return s
}

func TestDeployNeedsOneCoveringGrant(t *testing.T) {
	p := Principal{ID: "node:a", Grants: []Grant{
		{Role: Deployer, Services: []string{"preview-*"}, Expose: []string{"tailnet"}, MaxTTL: "24h"},
		{Role: Deployer, Services: []string{"blog"}, Expose: []string{"funnel"}},
		{Role: Viewer, Services: []string{"*"}},
	}}
	cases := []struct {
		s    spec.Stack
		want bool
	}{
		{stack("preview-1", "tailnet", "1h"), true},
		{stack("preview-1", "tailnet", "48h"), false},
		{stack("preview-1", "tailnet", ""), false},
		{stack("preview-1", "funnel", "1h"), false},
		{stack("blog", "funnel", ""), true},
		{stack("blog", "tailnet", ""), false},
		{stack("other", "tailnet", ""), false},
	}
	for _, c := range cases {
		if got := p.CanDeploy(c.s); got != c.want {
			t.Errorf("%s/%s/%v: got %v", c.s.Name, c.s.Expose, c.s.TTL, got)
		}
	}
	if !p.CanRead("anything") || p.IsAdmin() {
		t.Fatal("read or admin")
	}
	if !p.CanDelete("preview-1", true, "node:a") || p.CanDelete("preview-1", true, "node:b") || p.CanDelete("blog", false, "node:a") {
		t.Fatal("delete rules")
	}
}
