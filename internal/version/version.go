// Package version reports the build version, set at link time:
// -ldflags "-X github.com/quinnovator/inhouse/internal/version.Version=v1.2.3".
package version

import "fmt"

var Version = "dev"

// Header carries the daemon's version on every response it serves.
const Header = "Inhouse-Version"

// Mismatch returns a warning when this build and the daemon's differ, or ""
// when they match or this is a development build. An empty daemon version
// means the daemon predates version reporting.
func Mismatch(daemon string) string {
	switch {
	case Version == "dev" || daemon == Version:
		return ""
	case daemon == "":
		return fmt.Sprintf("warning: this is inhouse %s but the daemon does not report its version, so it is older; upgrade it", Version)
	case daemon == "dev":
		return ""
	}
	return fmt.Sprintf("warning: this is inhouse %s but the daemon is %s; upgrade whichever is older so they match", Version, daemon)
}
