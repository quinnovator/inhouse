// Package tailnet joins embedded Tailscale nodes (tsnet) to the tailnet and
// talks to the Tailscale API to enroll and delete them.
//
// Two OAuth clients keep authority separate:
//
//   - enroll (scope auth_keys, owner tag tag:inhouse-enroll) mints a
//     single-use, pre-authorized auth key per node, tagged for its class.
//   - lifecycle (scope devices:core) deletes a service's device when the
//     service is deleted, after checking the device is exactly the one
//     inhouse created.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

// Node tags. tag:inhouse-enroll owns the three node classes so one OAuth
// client can mint keys for each class separately.
const (
	TagControl   = "tag:inhouse-control"
	TagService   = "tag:inhouse-svc"
	TagEphemeral = "tag:inhouse-eph"
	TagAgent     = "tag:inhouse-agent"
)

// Credential is an OAuth client: its ID and a file holding its secret.
type Credential struct {
	ClientID   string
	SecretFile string
}

func (c Credential) secret() (string, error) {
	if c.ClientID == "" || c.SecretFile == "" {
		return "", errors.New("OAuth client ID and secret file are required")
	}
	raw, err := os.ReadFile(c.SecretFile)
	if err != nil {
		return "", fmt.Errorf("cannot read OAuth secret: %w", err)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", errors.New("OAuth secret file is empty")
	}
	return s, nil
}

// NodeConfig describes one embedded node.
type NodeConfig struct {
	Dir       string // private state directory; keeps the node's identity across restarts
	Hostname  string
	Tag       string
	Ephemeral bool
	Enroll    Credential
	// Certificate prepares the node's HTTPS certificate before returning and
	// records its identity for later deletion.
	Certificate bool
	// ControlURL overrides the coordination server (tests only).
	ControlURL string
	// AuthKey skips minting (tests only).
	AuthKey string
}

type Node struct {
	Server *tsnet.Server
	Client *local.Client
	DNS    string // MagicDNS name, without the trailing dot
}

// Identity is what inhouse records about a node it created, so it can later
// prove a device is that node before deleting it.
type Identity struct {
	DeviceID string `json:"device_id"`
	StableID string `json:"stable_id"`
	DNS      string `json:"dns"`
	Tag      string `json:"tag"`
}

const identityFile = "identity.json"

// Open starts a node, enrolling it with a fresh key if it has no saved state.
// Ephemeral nodes always get a fresh key: Tailscale may have removed them
// while the daemon was down.
func Open(ctx context.Context, c NodeConfig) (*Node, error) {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(c.Dir, 0o700); err != nil {
		return nil, err
	}
	srv := &tsnet.Server{
		Dir:           c.Dir,
		Hostname:      c.Hostname,
		AdvertiseTags: []string{c.Tag},
		Ephemeral:     c.Ephemeral,
		ControlURL:    c.ControlURL,
		UserLogf:      func(string, ...any) {},
	}
	failed := func(err error) (*Node, error) { _ = srv.Close(); return nil, err }
	_, err := os.Stat(filepath.Join(c.Dir, "tailscaled.state"))
	switch {
	case c.AuthKey != "":
		srv.AuthKey = c.AuthKey
	case errors.Is(err, os.ErrNotExist) || c.Ephemeral:
		if srv.AuthKey, err = MintKey(ctx, c.Enroll, c.Tag, c.Ephemeral); err != nil {
			return failed(err)
		}
	case err != nil:
		return failed(err)
	}
	up, cancel := context.WithTimeout(ctx, 2*time.Minute)
	status, err := srv.Up(up)
	cancel()
	srv.AuthKey = ""
	if err != nil {
		return failed(fmt.Errorf("node %s did not come online: %w", c.Hostname, err))
	}
	if status.Self == nil || status.Self.DNSName == "" {
		return failed(errors.New("node has no MagicDNS name; enable MagicDNS and HTTPS certificates"))
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return failed(err)
	}
	dns := strings.TrimSuffix(status.Self.DNSName, ".")
	if c.Certificate {
		id := Identity{DeviceID: fmt.Sprint(status.Self.NodeID), StableID: string(status.Self.ID), DNS: dns, Tag: c.Tag}
		raw, _ := json.Marshal(id)
		if err = os.WriteFile(filepath.Join(c.Dir, identityFile), raw, 0o600); err != nil {
			return failed(err)
		}
		// The first certificate takes a few seconds; get it before serving.
		cert, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, _, err = lc.CertPair(cert, dns)
		cancel()
		if err != nil {
			return failed(fmt.Errorf("HTTPS certificate for %s is not ready: %w", dns, err))
		}
	}
	return &Node{srv, lc, dns}, nil
}

func (n *Node) Close() error { return n.Server.Close() }

// ReadIdentity loads the identity Open recorded in dir.
func ReadIdentity(dir string) (Identity, error) {
	var id Identity
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return id, err
	}
	return id, json.Unmarshal(raw, &id)
}
