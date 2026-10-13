package platform

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

const statSample = "cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 50 0 25 400 25 0 0 0 0 0\nintr 123\n"

func TestParseCPUTimes(t *testing.T) {
	got, err := parseCPUTimes(statSample)
	if err != nil {
		t.Fatal(err)
	}
	if got.total != 1000 || got.idle != 850 {
		t.Fatal(got)
	}
	if _, err = parseCPUTimes("cpu0 1 2 3\n"); err == nil {
		t.Fatal("no aggregate line accepted")
	}
	if _, err = parseCPUTimes("cpu 1 2 x 4 5 6\n"); err == nil {
		t.Fatal("bad counter accepted")
	}
}

func TestUtilization(t *testing.T) {
	prev := cpuTimes{total: 1000, idle: 850}
	if got, ok := utilization(prev, cpuTimes{total: 2000, idle: 1650}); !ok || got != 20 {
		t.Fatal(got, ok)
	}
	if _, ok := utilization(prev, prev); ok {
		t.Fatal("stalled counters produced a rate")
	}
	if _, ok := utilization(prev, cpuTimes{total: 900, idle: 800}); ok {
		t.Fatal("rewound counters produced a rate")
	}
}

func TestParseMemInfo(t *testing.T) {
	got, err := parseMemInfo("MemTotal:       16384000 kB\nMemFree:         1024000 kB\nMemAvailable:    8192000 kB\nSwapTotal:       2097152 kB\nSwapFree:        1048576 kB\nHugePages_Total: 0\n")
	want := memInfo{total: 16384000 * 1024, available: 8192000 * 1024, swapTotal: 2097152 * 1024, swapFree: 1048576 * 1024}
	if err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
	zero, err := parseMemInfo("MemTotal: 1000 kB\nMemAvailable: 0 kB\nMemFree: 250 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	if err != nil || zero.available != 0 {
		t.Fatal("measured zero availability replaced", zero, err)
	}
}

func TestCPUDoesNotDoubleCountGuestTime(t *testing.T) {
	got, err := parseCPUTimes("cpu 100 20 50 800 50 10 10 10 40 5\n")
	if err != nil || got.total != 1050 || got.idle != 850 {
		t.Fatal(got, err)
	}
}

func TestMemoryRejectsUnknownOrInvalidValues(t *testing.T) {
	valid := "MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 100 kB\nSwapFree: 50 kB\n"
	for _, raw := range []string{
		"MemTotal: 1000 kB\nMemFree: 250 kB\n",
		strings.Replace(valid, "250 kB", "bad kB", 1),
		strings.Replace(valid, "250 kB", "250 bytes", 1),
		strings.Replace(valid, "250 kB", "1001 kB", 1),
		strings.Replace(valid, "250 kB", "18446744073709551615 kB", 1),
		strings.Replace(valid, "50 kB", "101 kB", 1),
	} {
		if _, err := parseMemInfo(raw); err == nil {
			t.Fatalf("invalid memory accepted: %q", raw)
		}
	}
}

func TestParsePressure(t *testing.T) {
	got, ok := parsePressure("some avg10=1.50 avg60=0.20 avg300=0.01 total=12345\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	if !ok || got != 1.5 {
		t.Fatal(got, ok)
	}
	if _, ok = parsePressure(""); ok {
		t.Fatal("empty pressure accepted")
	}
	for _, value := range []string{"NaN", "+Inf", "-Inf", "-0.1", "100.1", "bad"} {
		if _, ok := parsePressure("some avg10=" + value); ok {
			t.Fatalf("invalid pressure accepted: %s", value)
		}
	}
	if got, ok := parsePressure("some avg10=0.00"); !ok || got != 0 {
		t.Fatal("valid zero pressure rejected", got, ok)
	}
}

func TestCPUFailureRequiresFreshBaseline(t *testing.T) {
	s, _ := fakes()
	s.Collect()
	s.src.cpuTimes = func() (cpuTimes, error) { return cpuTimes{}, errors.New("failed") }
	if got := s.Collect(); got.CPU.Status != StatusUnavailable {
		t.Fatal(got.CPU)
	}
	s.src.cpuTimes = func() (cpuTimes, error) { return cpuTimes{total: 2000, idle: 1650}, nil }
	if got := s.Collect(); got.CPU.Status != StatusWarmingUp {
		t.Fatal("CPU rate spans a failed reading", got.CPU)
	}
}

func fakes() (*sampler, *cpuTimes) {
	current := cpuTimes{total: 1000, idle: 850}
	s := &sampler{src: sources{
		cpuTimes: func() (cpuTimes, error) { return current, nil },
		memInfo: func() (memInfo, error) {
			return memInfo{total: 1000, available: 400, swapTotal: 100, swapFree: 90}, nil
		},
		pressure: func() (float64, bool) { return 2.5, true },
		statfs:   func() (uint64, uint64, uint64, error) { return 1000, 300, 250, nil },
	}}
	return s, &current
}

func TestSamplerWarmsUpThenMeasures(t *testing.T) {
	s, current := fakes()
	first := s.Collect()
	if first.CPU.Status != StatusWarmingUp || first.CPU.UtilizationPercent != nil {
		t.Fatalf("first sample invented CPU use: %+v", first.CPU)
	}
	if first.Storage.Status != StatusOK || *first.Storage.AvailableBytes != 250 {
		t.Fatalf("%+v", first.Storage)
	}
	if first.Memory.Status != StatusOK || *first.Memory.UsedBytes != 600 || *first.Memory.PressureSomeAvg10Percent != 2.5 {
		t.Fatalf("%+v", first.Memory)
	}
	*current = cpuTimes{total: 2000, idle: 1650}
	second := s.Collect()
	if second.CPU.Status != StatusOK || *second.CPU.UtilizationPercent != 20 {
		t.Fatalf("%+v", second.CPU)
	}
	if second.OS != runtime.GOOS || second.Arch != runtime.GOARCH || second.LogicalCPUs != runtime.NumCPU() || second.SampledAt == 0 {
		t.Fatalf("%+v", second)
	}
	// Counters going backwards warm up again instead of reporting a rate.
	*current = cpuTimes{total: 10, idle: 5}
	if got := s.Collect(); got.CPU.Status != StatusWarmingUp || got.CPU.UtilizationPercent != nil {
		t.Fatalf("%+v", got.CPU)
	}
}

func TestSamplerReportsFailuresPerMetric(t *testing.T) {
	s, _ := fakes()
	s.src.statfs = func() (uint64, uint64, uint64, error) { return 0, 0, 0, errors.New("statfs /var/lib/inhouse: gone") }
	s.src.memInfo = func() (memInfo, error) { return memInfo{}, errors.New("read /proc/meminfo: gone") }
	got := s.Collect()
	if got.Storage.Status != StatusUnavailable || got.Storage.TotalBytes != nil {
		t.Fatalf("%+v", got.Storage)
	}
	if got.Memory.Status != StatusUnavailable || got.Memory.UsedBytes != nil {
		t.Fatalf("%+v", got.Memory)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/var/lib/inhouse") || strings.Contains(string(raw), "/proc") {
		t.Fatalf("snapshot leaks host paths: %s", raw)
	}
}

func TestUnsupportedReportsNoMetrics(t *testing.T) {
	got := Unsupported{}.Collect()
	if got.Storage.Status != StatusUnavailable || got.CPU.Status != StatusUnavailable || got.Memory.Status != StatusUnavailable {
		t.Fatalf("%+v", got)
	}
	if got.Storage.TotalBytes != nil || got.CPU.UtilizationPercent != nil || got.Memory.TotalBytes != nil {
		t.Fatal("unsupported sample carries values")
	}
	if got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("%+v", got)
	}
	if c := New("/nonexistent-data-root"); c == nil {
		t.Fatal("New returned nil")
	} else if snap := c.Collect(); snap.SampledAt == 0 {
		t.Fatal("New's collector does not sample")
	}
}
