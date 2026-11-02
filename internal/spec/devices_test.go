package spec

import "testing"

func TestDevicesAllowlist(t *testing.T) {
	for _, tc := range []struct {
		devices string
		valid   bool
	}{
		{"[/dev/net/tun]", true}, {"[/dev/kvm]", true}, {"[/dev/net/tun, /dev/kvm]", true},
		{"[/dev/sda]", false}, {"[/dev/net/tun, /dev/net/tun]", false}, {"[/dev/kvm, /dev/kvm]", false},
		{"[/dev/net/tun:rw]", false}, {"[/dev/kvm:rwm]", false}, {"[/dev/vhost-vsock]", false}, {"[/dev/userfaultfd]", false},
	} {
		_, err := Parse([]byte("name: vpn\ncontainers:\n  app:\n    image: docker.io/library/alpine:3\n    port: 80\n    devices: " + tc.devices + "\n"))
		if (err == nil) != tc.valid {
			t.Fatalf("devices %s: %v", tc.devices, err)
		}
	}
}
