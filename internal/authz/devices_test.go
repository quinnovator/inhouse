package authz

import (
	"github.com/quinnovator/inhouse/internal/spec"
	"testing"
)

func TestDeviceAccessRequiresAdministrator(t *testing.T) {
	for _, device := range []string{"/dev/net/tun", "/dev/kvm"} {
		s := spec.Stack{Name: "preview-test", Expose: "tailnet", TTL: new("1h"), Containers: map[string]spec.Container{"app": {Devices: []string{device}}}}
		if (Grant{Role: Deployer, Services: []string{"preview-*"}, Expose: []string{"tailnet"}, MaxTTL: "24h"}).covers(s) {
			t.Fatal("preview granted device access", device)
		}
		if !(Grant{Role: Admin}).covers(s) {
			t.Fatal("administrator refused", device)
		}
	}
}
