// Package spec parses and validates stack specs: the YAML or JSON documents
// that describe a service's containers.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// MaxBytes bounds a spec document.
const MaxBytes = 64 << 10

const (
	ExposeTailnet = "tailnet"
	ExposeFunnel  = "funnel"

	Rolling  = "rolling"
	Recreate = "recreate"
)

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)
	envRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	digestRE = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)
)

// ValidName reports whether s is a DNS label of 1–40 lowercase letters,
// digits or hyphens. Services, containers, volumes and secrets share it.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Pinned reports whether an image reference names an exact digest.
func Pinned(image string) bool { return digestRE.MatchString(image) }

type Stack struct {
	Name           string               `json:"name" yaml:"name"`
	Expose         string               `json:"expose" yaml:"expose"`
	TTL            *string              `json:"ttl,omitempty" yaml:"ttl"`
	UpdateStrategy string               `json:"update_strategy,omitempty" yaml:"update_strategy"`
	Containers     map[string]Container `json:"containers" yaml:"containers"`
	// Order is the containers' document order. YAML maps are unordered in Go,
	// so Parse records it from the document itself.
	Order []string `json:"order" yaml:"-"`
}

type Container struct {
	Image     string            `json:"image" yaml:"image"`
	Port      int               `json:"port,omitempty" yaml:"port"`
	Command   []string          `json:"command,omitempty" yaml:"command"`
	Args      []string          `json:"args,omitempty" yaml:"args"`
	Env       map[string]string `json:"env,omitempty" yaml:"env"`
	Secrets   map[string]string `json:"secrets,omitempty" yaml:"secrets"`
	Volumes   map[string]string `json:"volumes,omitempty" yaml:"volumes"`
	Health    Health            `json:"health" yaml:"health"`
	Resources Resources         `json:"resources" yaml:"resources"`
	// SecretVersions pins each referenced secret to the hash of the exact
	// ciphertext a revision was created with. It is set by the platform.
	SecretVersions map[string]string `json:"secret_versions,omitempty" yaml:"-"`
}

type Health struct {
	Path    string   `json:"path,omitempty" yaml:"path"`
	Command []string `json:"command,omitempty" yaml:"command"`
	Timeout string   `json:"timeout,omitempty" yaml:"timeout"`
}

type Resources struct {
	Memory string  `json:"memory" yaml:"memory"`
	CPUs   float64 `json:"cpus" yaml:"cpus"`
}

// Parse decodes exactly one YAML or JSON document, rejects unknown fields,
// applies defaults and validates the result.
func Parse(raw []byte) (Stack, error) {
	var s Stack
	if len(raw) > MaxBytes {
		return s, errors.New("spec exceeds 64 KiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("spec: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return s, errors.New("spec must contain exactly one document")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return s, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return s, errors.New("spec must be a mapping")
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "containers" {
			continue
		}
		containers := root.Content[i+1]
		if containers.Kind != yaml.MappingNode {
			return s, errors.New("containers must be a mapping")
		}
		for j := 0; j < len(containers.Content); j += 2 {
			s.Order = append(s.Order, containers.Content[j].Value)
		}
	}
	return s, s.Validate()
}

// Validate checks every rule and fills in defaults.
func (s *Stack) Validate() error {
	if !ValidName(s.Name) {
		return errors.New("name: must be a DNS label of 1–40 lowercase letters, digits or hyphens")
	}
	if s.Expose == "" {
		s.Expose = ExposeTailnet
	}
	if s.Expose != ExposeTailnet && s.Expose != ExposeFunnel {
		return errors.New("expose: expected tailnet or funnel")
	}
	if s.TTL != nil {
		ttl, err := time.ParseDuration(*s.TTL)
		if err != nil || ttl < time.Second || ttl > 720*time.Hour {
			return errors.New("ttl: expected a duration from 1s to 720h")
		}
		if s.Expose == ExposeFunnel {
			return errors.New("ttl: ephemeral stacks cannot use Funnel")
		}
	}
	if len(s.Containers) == 0 || len(s.Containers) > 16 {
		return errors.New("containers: expected 1–16 containers")
	}
	if len(s.Order) != len(s.Containers) {
		return errors.New("containers: start order is missing")
	}
	seen := map[string]bool{}
	ingress, hasVolumes := 0, false
	for _, name := range s.Order {
		c, ok := s.Containers[name]
		if !ok || seen[name] || !ValidName(name) {
			return errors.New("containers: invalid or duplicate container names")
		}
		seen[name] = true
		if err := c.validate(name); err != nil {
			return err
		}
		if c.Port > 0 {
			ingress++
		}
		if len(c.Volumes) > 0 {
			hasVolumes = true
		}
		s.Containers[name] = c
	}
	if ingress != 1 {
		return errors.New("containers: exactly one container must set port (the ingress)")
	}
	if s.UpdateStrategy == "" {
		s.UpdateStrategy = Rolling
		if hasVolumes {
			s.UpdateStrategy = Recreate
		}
	}
	if s.UpdateStrategy != Rolling && s.UpdateStrategy != Recreate {
		return errors.New("update_strategy: expected rolling or recreate")
	}
	if hasVolumes && s.UpdateStrategy != Recreate {
		return errors.New("update_strategy: stacks with volumes must use recreate, so two revisions never write the same data")
	}
	return nil
}

func (c *Container) validate(name string) error {
	field := func(f string) string { return "containers." + name + "." + f }
	host, _, _ := strings.Cut(c.Image, "/")
	if c.Image == "" || strings.ContainsAny(c.Image, " \t\r\n") || !strings.Contains(c.Image, "/") || !strings.ContainsAny(host, ".:") {
		return fmt.Errorf("%s: use a fully qualified reference such as docker.io/library/nginx:1", field("image"))
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("%s: expected 1–65535", field("port"))
	}
	if len(c.Env) > 128 {
		return fmt.Errorf("%s: at most 128 entries", field("env"))
	}
	for k, v := range c.Env {
		if !envRE.MatchString(k) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("%s: invalid entry %q", field("env"), k)
		}
	}
	if len(c.Secrets) > 128 {
		return fmt.Errorf("%s: at most 128 entries", field("secrets"))
	}
	for env, secret := range c.Secrets {
		if !envRE.MatchString(env) || !ValidName(secret) {
			return fmt.Errorf("%s: invalid reference %q", field("secrets"), env)
		}
		if _, clash := c.Env[env]; clash {
			return fmt.Errorf("%s: %s is also set in env", field("secrets"), env)
		}
	}
	if len(c.Volumes) > 32 {
		return fmt.Errorf("%s: at most 32 entries", field("volumes"))
	}
	targets := map[string]bool{}
	for volume, target := range c.Volumes {
		if !ValidName(volume) || !strings.HasPrefix(target, "/") || path.Clean(target) != target || target == "/" || strings.ContainsAny(target, ":\x00\n") || targets[target] {
			return fmt.Errorf("%s: invalid volume name or mount path for %q", field("volumes"), volume)
		}
		targets[target] = true
	}
	if c.Health.Path != "" && (c.Port == 0 || !strings.HasPrefix(c.Health.Path, "/") || len(c.Health.Command) > 0) {
		return fmt.Errorf("%s: path checks apply only to the ingress and exclude command", field("health"))
	}
	if c.Health.Timeout == "" {
		c.Health.Timeout = "60s"
	}
	if t, err := time.ParseDuration(c.Health.Timeout); err != nil || t < 5*time.Second || t > 120*time.Second {
		return fmt.Errorf("%s: expected 5s–120s", field("health.timeout"))
	}
	if c.Resources.Memory == "" {
		c.Resources.Memory = "512Mi"
	}
	if m, err := Memory(c.Resources.Memory); err != nil || m < 4<<20 || m > 64<<30 {
		return fmt.Errorf("%s: expected 4Mi–64Gi", field("resources.memory"))
	}
	if c.Resources.CPUs == 0 {
		c.Resources.CPUs = 1
	}
	if math.IsNaN(c.Resources.CPUs) || math.IsInf(c.Resources.CPUs, 0) || c.Resources.CPUs < 0.01 || c.Resources.CPUs > 64 {
		return fmt.Errorf("%s: expected 0.01–64", field("resources.cpus"))
	}
	for _, args := range [][]string{c.Command, c.Args, c.Health.Command} {
		if len(args) > 128 {
			return fmt.Errorf("containers.%s: at most 128 command arguments", name)
		}
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return fmt.Errorf("containers.%s: command arguments cannot contain NUL", name)
			}
		}
	}
	return nil
}

// Memory parses a quantity such as 512Mi, 1G or 1048576 into bytes.
func Memory(s string) (int64, error) {
	units := []struct {
		suffix string
		n      int64
	}{{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}, {"G", 1e9}, {"M", 1e6}, {"K", 1e3}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
			if err != nil || n < 0 || n > math.MaxInt64/u.n {
				return 0, errors.New("invalid memory quantity")
			}
			return n * u.n, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// StartOrder lists sidecars in document order, then the ingress last.
func (s Stack) StartOrder() []string {
	out := []string{}
	ingress := ""
	for _, n := range s.Order {
		if s.Containers[n].Port > 0 {
			ingress = n
		} else {
			out = append(out, n)
		}
	}
	return append(out, ingress)
}

// Ingress returns the one container that receives traffic.
func (s Stack) Ingress() (string, Container) {
	for _, n := range s.Order {
		if c := s.Containers[n]; c.Port > 0 {
			return n, c
		}
	}
	return "", Container{}
}

// VolumeNames lists each distinct volume the stack mounts.
func (s Stack) VolumeNames() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range s.Order {
		for v := range s.Containers[n].Volumes {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// Hash identifies a stack's exact content. Identical normalized stacks hash
// identically, which makes repeated deploys no-ops.
func (s Stack) Hash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TTLDuration returns the parsed TTL, or zero for persistent stacks.
func (s Stack) TTLDuration() time.Duration {
	if s.TTL == nil {
		return 0
	}
	d, _ := time.ParseDuration(*s.TTL)
	return d
}
