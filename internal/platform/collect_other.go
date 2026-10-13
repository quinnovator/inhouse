//go:build !linux

package platform

// New returns the unsupported collector: inhoused only runs in
// production on Linux, and local builds elsewhere report their host
// truthfully with every metric unavailable rather than guessed at.
func New(string) Collector { return Unsupported{} }
