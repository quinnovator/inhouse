package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
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
