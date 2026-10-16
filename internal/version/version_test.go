package version

import (
	"strings"
	"testing"
)

func TestMismatch(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "1.2.0"
	for daemon, warn := range map[string]bool{"1.2.0": false, "dev": false, "1.3.0": true, "": true} {
		if got := Mismatch(daemon); (got != "") != warn {
			t.Errorf("daemon %q: %q", daemon, got)
		}
	}
	if !strings.Contains(Mismatch("1.3.0"), "1.3.0") {
		t.Error("the warning should name the daemon's version")
	}
	Version = "dev"
	if got := Mismatch("1.3.0"); got != "" {
		t.Error("a development build warned:", got)
	}
}
