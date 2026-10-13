// Package platform samples the single host the daemon runs on: its name
// and platform, the storage available on the data root, CPU use and
// memory pressure.
//
// A sample never invents a number. Each metric group carries a status,
// and values are present only when they were really measured: CPU use
// needs two samples, so the first is "warming_up", and hosts that can't
// be sampled (anything but Linux) report "unavailable". Reasons are
// short codes, never errors, so no filesystem path or other host detail
// leaks into the API.
package platform

import (
	"errors"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metric statuses. Values accompany StatusOK only.
const (
	StatusOK          = "ok"
	StatusUnavailable = "unavailable"
	StatusWarmingUp   = "warming_up"
)

// Reasons say why a metric has no value, without host detail.
const (
	ReasonUnsupported = "unsupported_platform"
	ReasonFirstSample = "first_sample"
	ReasonCollect     = "collection_failed"
)

// Storage is the space on the filesystem holding the data root.
// Available is what an unprivileged process may still write; it is
// normally less than Free, which includes blocks reserved for root.
type Storage struct {
	Status         string  `json:"status"`
	Reason         string  `json:"reason,omitempty"`
	TotalBytes     *uint64 `json:"total_bytes,omitempty"`
	FreeBytes      *uint64 `json:"free_bytes,omitempty"`
	AvailableBytes *uint64 `json:"available_bytes,omitempty"`
}

// CPU is whole-host CPU use since the previous sample.
type CPU struct {
	Status             string   `json:"status"`
	Reason             string   `json:"reason,omitempty"`
	UtilizationPercent *float64 `json:"utilization_percent,omitempty"`
}

// Memory is whole-host memory use and pressure. Used is total minus
// what the kernel says is available for new work without swapping.
// PressureSomeAvg10 is the Linux pressure-stall average over the last
// ten seconds (the share of time some task stalled on memory), when
// the kernel reports it.
type Memory struct {
	Status                   string   `json:"status"`
	Reason                   string   `json:"reason,omitempty"`
	TotalBytes               *uint64  `json:"total_bytes,omitempty"`
	AvailableBytes           *uint64  `json:"available_bytes,omitempty"`
	UsedBytes                *uint64  `json:"used_bytes,omitempty"`
	SwapTotalBytes           *uint64  `json:"swap_total_bytes,omitempty"`
	SwapFreeBytes            *uint64  `json:"swap_free_bytes,omitempty"`
	PressureSomeAvg10Percent *float64 `json:"pressure_some_avg10_percent,omitempty"`
}

// Snapshot is one host sample. Hostname, OS, Arch and LogicalCPUs are
// facts about the running binary's host and are always reported
// truthfully (Hostname is empty only if the OS won't say).
type Snapshot struct {
	Hostname    string  `json:"hostname,omitempty"`
	OS          string  `json:"os"`
	Arch        string  `json:"arch"`
	LogicalCPUs int     `json:"logical_cpus"`
	Storage     Storage `json:"storage"`
	CPU         CPU     `json:"cpu"`
	Memory      Memory  `json:"memory"`
	SampledAt   int64   `json:"sampled_at"`
}

// Collector samples the host. Collect must be safe for concurrent use
// and quick: it runs inside an API request.
type Collector interface {
	Collect() Snapshot
}

func base() Snapshot {
	hostname, _ := os.Hostname()
	return Snapshot{
		Hostname:    hostname,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		LogicalCPUs: runtime.NumCPU(),
		SampledAt:   time.Now().Unix(),
	}
}

// Unsupported reports the host's identity but no metrics. It is the
// collector on non-Linux builds (local development) and the engine's
// default before a real collector is installed.
type Unsupported struct{}

func (Unsupported) Collect() Snapshot {
	s := base()
	s.Storage = Storage{Status: StatusUnavailable, Reason: ReasonUnsupported}
	s.CPU = CPU{Status: StatusUnavailable, Reason: ReasonUnsupported}
	s.Memory = Memory{Status: StatusUnavailable, Reason: ReasonUnsupported}
	return s
}

// sources are the raw readings one sample needs. The Linux collector
// wires them to /proc and statfs; tests wire them to fixed values.
type sources struct {
	cpuTimes func() (cpuTimes, error)
	memInfo  func() (memInfo, error)
	// pressure returns the "some avg10" percentage; ok is false when
	// the kernel doesn't report pressure. That never fails the sample.
	pressure func() (value float64, ok bool)
	statfs   func() (total, free, available uint64, err error)
}

// sampler turns sources into snapshots, remembering the previous CPU
// counters so utilization is measured between samples.
type sampler struct {
	src sources

	mu   sync.Mutex
	prev *cpuTimes
}

func (s *sampler) Collect() Snapshot {
	out := base()
	out.Storage = s.storage()
	out.CPU = s.cpu()
	out.Memory = s.memory()
	return out
}

func (s *sampler) storage() Storage {
	total, free, available, err := s.src.statfs()
	if err != nil || total == 0 || free > total || available > free {
		return Storage{Status: StatusUnavailable, Reason: ReasonCollect}
	}
	return Storage{Status: StatusOK, TotalBytes: &total, FreeBytes: &free, AvailableBytes: &available}
}

func (s *sampler) cpu() CPU {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, err := s.src.cpuTimes()
	if err != nil {
		s.prev = nil
		return CPU{Status: StatusUnavailable, Reason: ReasonCollect}
	}
	prev := s.prev
	s.prev = &now
	if prev == nil {
		return CPU{Status: StatusWarmingUp, Reason: ReasonFirstSample}
	}
	percent, ok := utilization(*prev, now)
	if !ok {
		// Counters stalled or went backwards (e.g. a reboot between
		// samples): no truthful rate exists for this interval.
		return CPU{Status: StatusWarmingUp, Reason: ReasonFirstSample}
	}
	return CPU{Status: StatusOK, UtilizationPercent: &percent}
}

func (s *sampler) memory() Memory {
	info, err := s.src.memInfo()
	if err != nil || info.total == 0 || info.available > info.total || info.swapFree > info.swapTotal {
		return Memory{Status: StatusUnavailable, Reason: ReasonCollect}
	}
	used := info.total - min(info.available, info.total)
	m := Memory{
		Status:         StatusOK,
		TotalBytes:     &info.total,
		AvailableBytes: &info.available,
		UsedBytes:      &used,
		SwapTotalBytes: &info.swapTotal,
		SwapFreeBytes:  &info.swapFree,
	}
	if s.src.pressure != nil {
		if value, ok := s.src.pressure(); ok {
			m.PressureSomeAvg10Percent = &value
		}
	}
	return m
}

// cpuTimes are the aggregate counters from /proc/stat's "cpu" line,
// summed as the kernel accumulates them.
type cpuTimes struct {
	total uint64
	idle  uint64 // idle + iowait: time the CPU did no useful work
}

// utilization is the busy share of the interval between two counter
// readings, as a percentage. It reports false when the counters don't
// describe a forward interval.
func utilization(prev, now cpuTimes) (float64, bool) {
	if now.total <= prev.total || now.idle < prev.idle {
		return 0, false
	}
	total, idle := now.total-prev.total, now.idle-prev.idle
	if idle > total {
		return 0, false
	}
	return 100 * float64(total-idle) / float64(total), true
}

// parseCPUTimes reads /proc/stat content's aggregate "cpu" line:
// user, nice, system, idle, iowait and whatever follows.
func parseCPUTimes(content string) (cpuTimes, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		if len(fields) < 9 {
			return cpuTimes{}, errShort
		}
		var out cpuTimes
		// guest and guest_nice are already included in user and nice.
		// Sum user through steal once, excluding the guest counters.
		for i, field := range fields[1:9] {
			n, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}
			if n > math.MaxUint64-out.total {
				return cpuTimes{}, errShort
			}
			out.total += n
			if i == 3 || i == 4 { // idle, iowait
				out.idle += n
			}
		}
		return out, nil
	}
	return cpuTimes{}, errShort
}

type memInfo struct {
	total, available, swapTotal, swapFree uint64
}

// parseMemInfo reads the required KiB fields from /proc/meminfo.
// MemAvailable is distinct from MemFree, and a measured zero is valid.
func parseMemInfo(content string) (memInfo, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "MemTotal" && key != "MemAvailable" && key != "SwapTotal" && key != "SwapFree" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return memInfo{}, errors.New("invalid memory field")
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || n > math.MaxUint64/1024 {
			return memInfo{}, errors.New("invalid memory value")
		}
		values[key] = n * 1024
	}
	for _, key := range []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"} {
		if _, ok := values[key]; !ok {
			return memInfo{}, errors.New("missing memory field")
		}
	}
	info := memInfo{
		total:     values["MemTotal"],
		available: values["MemAvailable"],
		swapTotal: values["SwapTotal"],
		swapFree:  values["SwapFree"],
	}
	if info.total == 0 || info.available > info.total || info.swapFree > info.swapTotal {
		return memInfo{}, errors.New("inconsistent memory values")
	}
	return info, nil
}

// parsePressure reads /proc/pressure/memory content's "some" line and
// returns its avg10 percentage.
func parsePressure(content string) (float64, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "avg10="); ok {
				f, err := strconv.ParseFloat(value, 64)
				return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 100
			}
		}
	}
	return 0, false
}

type shortError struct{}

func (shortError) Error() string { return "unexpectedly short host data" }

var errShort = shortError{}
