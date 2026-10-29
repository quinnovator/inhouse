package spec

import "testing"

func TestDevicesAllowlist(t *testing.T) {
	for _, tc := range []struct {
		devices string
		valid   bool
	}{{"[/dev/net/tun]", true}, {"[/dev/sda]", false}, {"[/dev/net/tun, /dev/net/tun]", false}, {"[/dev/net/tun:rw]", false}} {
		_, err := Parse([]byte("name: vpn\ncontainers:\n  app:\n    image: docker.io/library/alpine:3\n    port: 80\n    devices: " + tc.devices + "\n"))
		if (err == nil) != tc.valid {
			t.Fatalf("devices %s: %v", tc.devices, err)
		}
	}
}
