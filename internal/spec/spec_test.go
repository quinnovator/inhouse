package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `name: blog
containers:
  db:
    image: docker.io/library/postgres:17
    volumes: {data: /var/lib/postgresql/data}
    health: {command: [pg_isready]}
  web:
    image: ghcr.io/example/blog:1.4.2
    port: 3000
    env: {DATABASE_URL: postgres://localhost/blog}
    secrets: {DB_PASSWORD: blog-db}
  cache:
    image: docker.io/library/redis:7
`

func TestParseDefaultsAndOrder(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Order, ",") != "db,web,cache" || strings.Join(s.StartOrder(), ",") != "db,cache,web" {
		t.Fatal(s.Order, s.StartOrder())
	}
	if s.Expose != ExposeTailnet || s.UpdateStrategy != Recreate {
		t.Fatal("defaults", s.Expose, s.UpdateStrategy)
	}
	web := s.Containers["web"]
	if web.Health.Timeout != "60s" || web.Resources.Memory != "512Mi" || web.Resources.CPUs != 1 {
		t.Fatal("container defaults", web)
	}
	if name, _ := s.Ingress(); name != "web" {
		t.Fatal(name)
	}
	again, _ := Parse([]byte(valid))
	if s.Hash() != again.Hash() {
		t.Fatal("hash is not deterministic")
	}
}

func TestParseJSON(t *testing.T) {
	s, err := Parse([]byte(`{"name":"x","containers":{"b":{"image":"docker.io/a/b:1"},"a":{"image":"docker.io/a/a:1","port":80}}}`))
	if err != nil || strings.Join(s.Order, ",") != "b,a" {
		t.Fatal(s.Order, err)
	}
}

func TestRejects(t *testing.T) {
	ingress := "containers:\n  web: {image: docker.io/a/b:1, port: 80}\n"
	for name, doc := range map[string]string{
		"bad name":            "name: Blog\n" + ingress,
		"unknown field":       "name: a\nreplicas: 2\n" + ingress,
		"order is internal":   "name: a\norder: [web]\n" + ingress,
		"two documents":       "name: a\n" + ingress + "---\nname: b\n",
		"no ingress":          "name: a\ncontainers:\n  web: {image: docker.io/a/b:1}\n",
		"two ingresses":       "name: a\ncontainers:\n  a: {image: docker.io/a/b:1, port: 80}\n  b: {image: docker.io/a/b:1, port: 81}\n",
		"unqualified image":   "name: a\ncontainers:\n  web: {image: nginx, port: 80}\n",
		"funnel ttl":          "name: a\nexpose: funnel\nttl: 1h\n" + ingress,
		"ttl too long":        "name: a\nttl: 1000h\n" + ingress,
		"rolling with volume": "name: a\nupdate_strategy: rolling\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, volumes: {data: /data}}\n",
		"relative mount":      "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, volumes: {data: data}}\n",
		"path on sidecar":     "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80}\n  side: {image: docker.io/a/c:1, health: {path: /}}\n",
		"short timeout":       "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, health: {timeout: 1s}}\n",
		"secret clashes env":  "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, env: {X: y}, secrets: {X: s}}\n",
		"tiny memory":         "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, resources: {memory: 1Ki}}\n",
		"newer version":       "version: 2\nname: a\n" + ingress,
		"version zero":        "version: 0\nname: a\n" + ingress,
		"version as text":     "version: one\nname: a\n" + ingress,
		"null version":        "version: null\nname: a\n" + ingress,
		"empty version":       "version:\nname: a\n" + ingress,
		"drain too long":      "name: a\ndrain: 48h\n" + ingress,
		"bad protocol":        "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, protocol: h3}\n",
		"sidecar protocol":    "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80}\n  side: {image: docker.io/a/c:1, protocol: h2c}\n",
		"raw port protocol":   "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 5000, protocol: sctp}]}\n",
		"raw https port":      "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 443, protocol: tcp}]}\n",
		"raw ingress port":    "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 80, protocol: tcp}]}\n",
		"raw port twice":      "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 5000, protocol: udp, count: 3}]}\n  side: {image: docker.io/a/c:1, ports: [{port: 5002, protocol: udp}]}\n",
		"range past 65535":    "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 65535, protocol: udp, count: 2}]}\n",
		"too many ports":      "name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 5000, protocol: udp, count: 40}, {port: 6000, protocol: udp, count: 40}]}\n",
		"root route":          "name: a\nroutes: [{path: /, container: web, port: 81}]\n" + ingress,
		"trailing slash":      "name: a\nroutes: [{path: /rtc/, container: web, port: 81}]\n" + ingress,
		"dot route":           "name: a\nroutes: [{path: /a/../rtc, container: web, port: 81}]\n" + ingress,
		"duplicate route":     "name: a\nroutes: [{path: /rtc, container: web, port: 81}, {path: /rtc, container: web, port: 82}]\n" + ingress,
		"route to nothing":    "name: a\nroutes: [{path: /rtc, container: nope, port: 81}]\n" + ingress,
		"route to ingress":    "name: a\nroutes: [{path: /rtc, container: web, port: 80}]\n" + ingress,
		"route protocols":     "name: a\nroutes: [{path: /a, container: web, port: 81}, {path: /b, container: web, port: 81, protocol: h2c}]\n" + ingress,
		"raw routed port":     "name: a\nroutes: [{path: /a, container: web, port: 81}]\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 81, protocol: tcp}]}\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRoutesAndPorts(t *testing.T) {
	s, err := Parse([]byte(`name: media
drain: 30m
routes:
  - {path: /rtc, container: sfu, port: 7880}
  - {path: /twirp, container: sfu, port: 7880, protocol: http}
  - {path: /pkg.Echo, container: grpc, port: 9000, protocol: h2c}
containers:
  sfu:
    image: docker.io/livekit/livekit-server:1
    ports:
      - {port: 7881, protocol: tcp}
      - {port: 7882, protocol: udp}
      - {port: 50000, protocol: udp, count: 3}
  grpc:
    image: docker.io/a/grpc:1
  web:
    image: docker.io/a/web:1
    port: 8080
    protocol: http
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.HostPorts(), ","); got != "http/7880,http/9000,tcp/7881,udp/7882,udp/50000,udp/50001,udp/50002" {
		t.Fatal(got)
	}
	if s.DrainDuration() != 30*time.Minute || s.Routes[1].Protocol != "" || s.Containers["web"].Protocol != "" {
		t.Fatal("drain or protocol defaults", s.DrainDuration(), s.Routes[1].Protocol, s.Containers["web"].Protocol)
	}
	// Writing a default spells out the same stack, so it must hash the same.
	explicit, _ := Parse([]byte("name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, protocol: http, ports: [{port: 9, protocol: udp, count: 1}]}\n"))
	implicit, _ := Parse([]byte("name: a\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80, ports: [{port: 9, protocol: udp}]}\n"))
	if explicit.Hash() != implicit.Hash() {
		t.Fatal("explicit defaults changed the hash")
	}
}

// Specs written before routes, ports, protocols and drain existed must store
// and hash exactly as they did, so redeploying them stays a no-op.
func TestNewFieldsLeaveOldSpecsUnchanged(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	for _, key := range []string{"routes", "ports", "protocol", "drain"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("stored spec gained %q: %s", key, raw)
		}
	}
	if len(s.HostPorts()) != 0 || s.DrainDuration() != 0 {
		t.Fatal(s.HostPorts(), s.DrainDuration())
	}
}

func TestVersion(t *testing.T) {
	bare, err := Parse([]byte(valid))
	if err != nil || bare.SpecVersion != nil {
		t.Fatal("a spec without a version should parse as unversioned", bare.SpecVersion, err)
	}
	versioned, err := Parse([]byte("version: 1\n" + valid))
	if err != nil || versioned.SpecVersion == nil || *versioned.SpecVersion != 1 {
		t.Fatal(versioned.SpecVersion, err)
	}
	// Revisions deployed before specs had a version were hashed without one,
	// so adding version: 1 must not turn a redeploy into a new revision.
	if bare.Hash() != versioned.Hash() {
		t.Fatal("version changed the hash")
	}
	if _, err = Parse([]byte(`{"version":1,"name":"x","containers":{"a":{"image":"docker.io/a/a:1","port":80}}}`)); err != nil {
		t.Fatal("JSON spec with a version:", err)
	}
	if _, err = Parse([]byte(`{"version":null,"name":"x","containers":{"a":{"image":"docker.io/a/a:1","port":80}}}`)); err == nil {
		t.Fatal("JSON spec with a null version was accepted")
	}
	_, err = Parse([]byte("version: 2\n" + valid))
	if err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatal("a newer version should ask for an upgrade:", err)
	}
}

func TestMemory(t *testing.T) {
	for in, want := range map[string]int64{"512Mi": 512 << 20, "1G": 1e9, "4096": 4096} {
		if got, err := Memory(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
}

func TestDeployInputSchemaIsJSON(t *testing.T) {
	if len(DeployInputSchema()) < 100 || !strings.Contains(string(Schema()), "containers") {
		t.Fatal("schema missing")
	}
}

func TestExamplesParse(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.yaml")
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Parse(raw); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
