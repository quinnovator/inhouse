//go:build linux

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

// New returns the Linux collector reporting storage for the filesystem
// that holds dataRoot (the daemon's configured data root). The path is
// only ever sampled, never reported.
func New(dataRoot string) Collector {
	read := func(path string) (string, error) {
		raw, err := os.ReadFile(path)
		return string(raw), err
	}
	return &sampler{src: sources{
		cpuTimes: func() (cpuTimes, error) {
			content, err := read("/proc/stat")
			if err != nil {
				return cpuTimes{}, err
			}
			return parseCPUTimes(content)
		},
		memInfo: func() (memInfo, error) {
			content, err := read("/proc/meminfo")
			if err != nil {
				return memInfo{}, err
			}
			return parseMemInfo(content)
		},
		pressure: func() (float64, bool) {
			content, err := read("/proc/pressure/memory")
			if err != nil {
				return 0, false
			}
			return parsePressure(content)
		},
		statfs: func() (uint64, uint64, uint64, error) {
			var st unix.Statfs_t
			if err := unix.Statfs(dataRoot, &st); err != nil {
				return 0, 0, 0, err
			}
			size := uint64(st.Bsize) //nolint:gosec // a block size is small and positive
			return st.Blocks * size, st.Bfree * size, st.Bavail * size, nil
		},
	}}
}
