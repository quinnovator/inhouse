// Package authz turns a tailnet caller into a principal with grants.
//
// There are no API tokens. The control node asks Tailscale who sent a request
// (WhoIs) and reads that caller's values for the instance's app capability
// from the tailnet policy file. Each value is one grant.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

var ErrForbidden = errors.New("permission denied by tailnet capability grant")

const (
	Viewer   = "viewer"
	Deployer = "deployer"
	Admin    = "admin"
)

// Grant is one capability value from the tailnet policy file.
type Grant struct {
	Role     string   `json:"role"`
	Services []string `json:"services,omitempty"`
	Expose   []string `json:"expose,omitempty"`
	MaxTTL   string   `json:"max_ttl,omitempty"`
}

// Principal is a verified caller: a person's login, or node:<stable id> for
// a tagged device.
type Principal struct {
	ID     string   `json:"principal"`
	Login  string   `json:"login,omitempty"`
	NodeID string   `json:"node_id"`
	Tags   []string `json:"tags,omitempty"`
	Grants []Grant  `json:"grants"`
}

// System is the principal the reconciler acts as.
var System = Principal{ID: "reconciler", Grants: []Grant{{Role: Admin}}}

type contextKey struct{}

func With(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

func From(ctx context.Context) Principal {
	p, _ := ctx.Value(contextKey{}).(Principal)
	return p
}

// FromWhoIs builds a principal from a WhoIs response. Grants that are
// malformed or carry unknown fields are dropped whole, never partly applied.
func FromWhoIs(w *apitype.WhoIsResponse, capability string) (Principal, error) {
	p := Principal{Grants: []Grant{}}
	if w == nil || w.Node == nil || capability == "" {
		return p, ErrForbidden
	}
	p.NodeID = string(w.Node.StableID)
	p.Tags = append([]string(nil), w.Node.Tags...)
	switch {
	case len(p.Tags) > 0 && p.NodeID != "":
		p.ID = "node:" + p.NodeID
	case len(p.Tags) == 0 && w.UserProfile != nil:
		p.Login = w.UserProfile.LoginName
		p.ID = p.Login
	}
	if p.ID == "" {
		return p, ErrForbidden
	}
	for _, raw := range w.CapMap[tailcfg.PeerCapability(capability)] {
		if g, ok := parseGrant([]byte(raw)); ok {
			p.Grants = append(p.Grants, g)
		}
	}
	if len(p.Grants) == 0 {
		return p, ErrForbidden
	}
	return p, nil
}

func parseGrant(raw []byte) (Grant, bool) {
	var g Grant
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&g) != nil {
		return g, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return g, false
	}
	return g, g.valid()
}

func (g Grant) valid() bool {
	if g.Role == Admin {
		return len(g.Services) == 0 && len(g.Expose) == 0 && g.MaxTTL == ""
	}
	if g.Role != Viewer && g.Role != Deployer {
		return false
	}
	if len(g.Services) == 0 || len(g.Services) > 64 {
		return false
	}
	for _, pattern := range g.Services {
		if strings.Contains(pattern, "/") {
			return false
		}
		if _, err := path.Match(pattern, "x"); err != nil {
			return false
		}
	}
	for _, mode := range g.Expose {
		if mode != spec.ExposeTailnet && mode != spec.ExposeFunnel {
			return false
		}
	}
	if g.MaxTTL != "" {
		if t, err := time.ParseDuration(g.MaxTTL); err != nil || t <= 0 {
			return false
		}
	}
	return true
}

func (g Grant) matches(service string) bool {
	if g.Role == Admin {
		return true
	}
	for _, pattern := range g.Services {
		if ok, _ := path.Match(pattern, service); ok {
			return true
		}
	}
	return false
}

// covers reports whether this one grant allows deploying s.
func (g Grant) covers(s spec.Stack) bool {
	if g.Role == Admin {
		return true
	}
	if g.Role != Deployer || !g.matches(s.Name) {
		return false
	}
	exposure := false
	for _, mode := range g.Expose {
		exposure = exposure || mode == s.Expose
	}
	if !exposure {
		return false
	}
	if g.MaxTTL != "" {
		limit, _ := time.ParseDuration(g.MaxTTL)
		ttl := s.TTLDuration()
		if ttl <= 0 || ttl > limit {
			return false
		}
	}
	return true
}

func (p Principal) Authenticated() bool { return p.ID != "" && len(p.Grants) > 0 }

func (p Principal) IsAdmin() bool {
	for _, g := range p.Grants {
		if g.Role == Admin {
			return true
		}
	}
	return false
}

// CanRead reports whether any grant names the service.
func (p Principal) CanRead(service string) bool {
	for _, g := range p.Grants {
		if g.matches(service) {
			return true
		}
	}
	return false
}

// CanDeploy requires one grant to cover name, exposure and TTL together.
func (p Principal) CanDeploy(s spec.Stack) bool {
	for _, g := range p.Grants {
		if g.covers(s) {
			return true
		}
	}
	return false
}

// CanDelete allows admins anything, and deployers their own ephemeral services.
func (p Principal) CanDelete(service string, ephemeral bool, createdBy string) bool {
	if p.IsAdmin() {
		return true
	}
	if !ephemeral || createdBy != p.ID {
		return false
	}
	for _, g := range p.Grants {
		if g.Role == Deployer && g.matches(service) {
			return true
		}
	}
	return false
}

func (p Principal) RequireRead(service string) error {
	if p.CanRead(service) {
		return nil
	}
	return fmt.Errorf("%w: cannot read service %s", ErrForbidden, service)
}

func (p Principal) RequireDeploy(s spec.Stack) error {
	if p.CanDeploy(s) {
		return nil
	}
	return fmt.Errorf("%w: no single deployer grant covers service %s with expose %s and this ttl", ErrForbidden, s.Name, s.Expose)
}

func (p Principal) RequireAdmin() error {
	if p.IsAdmin() {
		return nil
	}
	return fmt.Errorf("%w: admin role required", ErrForbidden)
}
