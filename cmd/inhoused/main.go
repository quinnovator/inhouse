// inhoused is the inhouse daemon. It runs as the unprivileged inhouse user
// and holds every tailnet node: one per service plus the control node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quinnovator/inhouse/internal/api"
	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/edge"
	"github.com/quinnovator/inhouse/internal/engine"
	"github.com/quinnovator/inhouse/internal/podman"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"github.com/quinnovator/inhouse/internal/userns"
	"github.com/quinnovator/inhouse/internal/vault"
	"github.com/quinnovator/inhouse/internal/version"
	"github.com/quinnovator/inhouse/internal/volumes"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// credentialFile defaults a secret path to the systemd credentials directory.
func credentialFile(name string) string {
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

func run() error {
	cfg := engine.DefaultConfig()
	dataDir := flag.String("data-dir", "/var/lib/inhouse", "persistent state root (btrfs)")
	socket := flag.String("podman-socket", filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "podman", "podman.sock"), "rootless Podman API socket")
	network := flag.String("podman-network", "pasta", "pod network mode: pasta or slirp4netns")
	capability := flag.String("capability", "", "app capability name in the tailnet policy, e.g. example.com/cap/inhouse (required)")
	enrollID := flag.String("enroll-client-id", "", "OAuth client ID that mints node auth keys (required)")
	enrollSecret := flag.String("enroll-secret-file", credentialFile("enroll-secret"), "file holding the enroll client's secret")
	lifecycleID := flag.String("lifecycle-client-id", "", "OAuth client ID that deletes service devices")
	lifecycleSecret := flag.String("lifecycle-secret-file", credentialFile("lifecycle-secret"), "file holding the lifecycle client's secret")
	flag.StringVar(&cfg.ControlName, "control-hostname", cfg.ControlName, "hostname of the control node")
	ports := flag.String("port-range", fmt.Sprintf("%d-%d", cfg.PortLow, cfg.PortHigh), "loopback ports pods publish their ingress on")
	subidBase := flag.Int("subid-base", 1000000, "first subordinate ID of the inhouse user (see /etc/subuid)")
	subidCount := flag.Int("subid-count", 4194304, "number of subordinate IDs the inhouse user owns")
	flag.DurationVar(&cfg.PullTimeout, "pull-timeout", cfg.PullTimeout, "how long pulling each image may take")
	flag.DurationVar(&cfg.HistoryRetention, "history-retention", cfg.HistoryRetention, "how long to keep events and finished operations; 0 keeps them forever")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}
	if *capability == "" || *enrollID == "" {
		return errors.New("-capability and -enroll-client-id are required")
	}
	if _, err := fmt.Sscanf(*ports, "%d-%d", &cfg.PortLow, &cfg.PortHigh); err != nil || cfg.PortLow < 1024 || cfg.PortHigh > 65535 || cfg.PortLow > cfg.PortHigh {
		return errors.New("-port-range must look like 20000-29999")
	}
	if cfg.PullTimeout <= 0 {
		return errors.New("-pull-timeout must be positive")
	}
	if cfg.HistoryRetention < 0 {
		return errors.New("-history-retention must not be negative")
	}
	if *subidCount < userns.Size {
		return errors.New("-subid-count must be at least " + strconv.Itoa(userns.Size))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := store.Open(filepath.Join(*dataDir, "state", "inhouse.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	secrets, err := vault.Open(db, filepath.Join(*dataDir, "keys", "age.key"))
	if err != nil {
		return err
	}
	ids := userns.Map{Store: db, Base: *subidBase, Count: *subidCount}
	vols := &volumes.Manager{Root: *dataDir, IDs: ids}
	pods := podman.New(*socket)
	defer pods.Close()
	pods.Offset, pods.Network, pods.Volumes, pods.Vault = ids.Offset, *network, vols, secrets

	enroll := tailnet.Credential{ClientID: *enrollID, SecretFile: *enrollSecret}
	lifecycle := tailnet.Credential{ClientID: *lifecycleID, SecretFile: *lifecycleSecret}
	edges := edge.NewManager(filepath.Join(*dataDir, "ts"), enroll, lifecycle)
	defer func() { _ = edges.Close() }()

	eng := engine.New(cfg, db, secrets, pods, vols, edges)
	reconciled := make(chan error, 1)
	go func() { reconciled <- eng.Run(ctx) }()
	defer func() { cancel(); <-reconciled }()

	// Services come back while the control node starts.
	control, err := tailnet.Open(ctx, tailnet.NodeConfig{
		Dir: filepath.Join(*dataDir, "ts", cfg.ControlName), Hostname: cfg.ControlName,
		Tag: tailnet.TagControl, Enroll: enroll, Certificate: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = control.Close() }()
	ip4, ip6 := control.Server.TailscaleIPs()
	server := &api.Server{Engine: eng, WhoIs: func(ctx context.Context, remote string) (authz.Principal, error) {
		// Capabilities are evaluated for this destination, so a grant to
		// tag:inhouse-control applies here and nowhere else.
		dst := ip4
		if strings.HasPrefix(remote, "[") {
			dst = ip6
		}
		who, err := control.Client.WhoIsForIP(ctx, remote, dst)
		if err != nil {
			return authz.Principal{}, err
		}
		return authz.FromWhoIs(who, *capability)
	}}
	listener, err := control.Server.ListenTLS("tcp", ":443")
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      engine.MaxWait + 10*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	log.Printf("inhouse %s control ready at https://%s (capability %s)", version.Version, control.DNS, *capability)
	select {
	case <-ctx.Done():
	case err = <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	return httpServer.Shutdown(shutdown)
}
