package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/platform"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/version"
)

type fakeCollector struct{ snap platform.Snapshot }

func (f fakeCollector) Collect() platform.Snapshot { return f.snap }

func viewer(services ...string) context.Context {
	return authz.With(context.Background(), authz.Principal{ID: "bob@example.com", Grants: []authz.Grant{{Role: authz.Viewer, Services: services}}})
}

func TestPlatformIsAdminOnly(t *testing.T) {
	h := setup(t)
	available := uint64(12345)
	h.SetPlatformCollector(fakeCollector{platform.Snapshot{
		Hostname: "inhouse", OS: "linux", Arch: "amd64", LogicalCPUs: 4,
		Storage: platform.Storage{Status: platform.StatusOK, AvailableBytes: &available},
		CPU:     platform.CPU{Status: platform.StatusWarmingUp, Reason: platform.ReasonFirstSample},
		Memory:  platform.Memory{Status: platform.StatusUnavailable, Reason: platform.ReasonCollect},
	}})
	for name, ctx := range map[string]context.Context{
		"viewer":   viewer("*"),
		"deployer": agent("node:agent"),
		"nobody":   context.Background(),
	} {
		if _, err := h.Platform(ctx); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("%s saw the platform: %v", name, err)
		}
	}
	op, err := h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:1", ""), "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.Platform(admin())
	if err != nil {
		t.Fatal(err)
	}
	if got.Daemon.Version != version.Version || got.Daemon.StartedAt <= 0 || got.Daemon.UptimeSeconds < 0 {
		t.Fatalf("%+v", got.Daemon)
	}
	if got.Host.Hostname != "inhouse" || got.Host.OS != "linux" || *got.Host.Storage.AvailableBytes != available {
		t.Fatalf("%+v", got.Host)
	}
	if got.Host.CPU.UtilizationPercent != nil || got.Host.Memory.TotalBytes != nil {
		t.Fatal("unmeasured metrics carry values")
	}
	if len(got.RunningOperations) != 1 || got.RunningOperations[0].ID != op.ID {
		t.Fatalf("%+v", got.RunningOperations)
	}
}

func TestGetShowsOnlyItsOwnRunningOperations(t *testing.T) {
	h := setup(t)
	hello, err := h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:1", ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Deploy(admin(), stack("blog", "docker.io/traefik/whoami:1", ""), ""); err != nil {
		t.Fatal(err)
	}
	bob := viewer("hello")
	detail, err := h.Get(bob, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Operations) != 1 || detail.Operations[0].ID != hello.ID || detail.Operations[0].State != store.Running {
		t.Fatalf("%+v", detail.Operations)
	}
	if _, err = h.Get(bob, "blog"); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("viewer read a service outside the grant: %v", err)
	}
	h.run(t, hello, nil)
	detail, err = h.Get(bob, "hello")
	if err != nil || len(detail.Operations) != 0 {
		t.Fatalf("finished operation still listed: %+v %v", detail.Operations, err)
	}
}
