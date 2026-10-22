package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// TestUpgradeFromEveryRelease migrates each release's database, as written
// by that release's own engine (see scripts/upgrade-fixture.sh), and checks
// that this daemon carries on where the old one stopped: nothing is lost,
// running work finishes, and redeploying an unchanged spec is still a no-op.
// The stacks a fixture names *.live.yaml are its services' live specs once
// the upgraded daemon has caught up.
func TestUpgradeFromEveryRelease(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "upgrade", "v*"))
	if err != nil || len(fixtures) == 0 {
		t.Fatal("no upgrade fixtures", err)
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) { upgradeFrom(t, fixture) })
	}
}

func upgradeFrom(t *testing.T, fixture string) {
	dir := t.TempDir()
	for from, to := range map[string]string{"inhouse.db": "state.db", "age.key": "age.key"} {
		raw, err := os.ReadFile(filepath.Join(fixture, from))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, to), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "state.db")
	before := rowCounts(t, file)
	from := schemaVersion(t, file)
	h := setupIn(t, dir)
	ctx := context.Background()

	// Opening backs up a database it migrates; a release with the current
	// schema needs neither.
	want := 0
	if from < schemaVersion(t, file) {
		want = 1
	}
	if b, _ := filepath.Glob(file + ".*.bak"); len(b) != want {
		t.Fatalf("expected %d pre-migration backups from schema %d, found %v", want, from, b)
	}
	// Migrations may add tables and columns, never lose rows.
	after := rowCounts(t, file)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows before migrating, %d after", table, n, after[table])
		}
	}
	// Every secret version deploys pinned still decrypts.
	hashes, err := column(file, "SELECT hash FROM secret_versions")
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range hashes {
		if _, err = h.vault.Version(ctx, hash); err != nil {
			t.Error("secret version", hash, err)
		}
	}
	// A live revision's spec, read back by this release, hashes as the old
	// release stored it, or redeploying it would roll out a new revision.
	services, err := h.db.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services {
		if s.Current == 0 {
			continue
		}
		r, err := h.db.Revision(ctx, s.Name, s.Current)
		if err != nil {
			t.Fatal(err)
		}
		if r.Spec.Hash() != r.Hash {
			t.Errorf("%s r%d: spec now hashes differently", s.Name, r.Rev)
		}
	}

	// Catch up as the daemon does on start.
	now := time.Now().Unix()
	for range 3 {
		for _, s := range services {
			if err = h.reconcile(ctx, s.Name); err != nil {
				t.Fatal(s.Name, err)
			}
		}
	}
	if running, err := h.db.RunningOperations(ctx, ""); err != nil || len(running) != 0 {
		t.Fatal("operations still running after the upgrade", running, err)
	}
	for _, s := range services {
		got, err := h.db.Service(ctx, s.Name)
		if s.Expires > 0 && s.Expires <= now {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s expired but survived the upgrade: %v", s.Name, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.Current == 0 || !h.rt.running[key(store.Revision{Service: got.Name, Rev: got.Current})] {
			t.Errorf("%s: live r%d isn't running after the upgrade", got.Name, got.Current)
		}
	}

	// Redeploying each live spec, and redeploying with secrets re-pinned,
	// changes nothing.
	live, err := filepath.Glob(filepath.Join(fixture, "stacks", "*.live.yaml"))
	if err != nil || len(live) == 0 {
		t.Fatal("fixture names no live specs", err)
	}
	for _, path := range live {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".live.yaml")
		stack, err := spec.Parse(raw)
		if err != nil {
			t.Fatal(name, err)
		}
		revs, _ := h.db.Revisions(ctx, stack.Name)
		o := h.deploy(t, raw, "redeploy-"+name)
		svc := h.service(t, o.Service)
		if again, _ := h.db.Revisions(ctx, stack.Name); o.State != store.Succeeded || o.Rev != svc.Current || len(again) != len(revs) {
			t.Errorf("%s: redeploying the live spec after the upgrade wasn't a no-op: %+v", name, o)
		}
		o, err = h.Redeploy(admin(), o.Service, "")
		if o = h.run(t, o, err); o.State != store.Succeeded || o.Rev != svc.Current {
			t.Errorf("%s: redeploy with unchanged secrets wasn't a no-op: %+v", name, o)
		}
	}
}

// rowCounts counts every table's rows.
func rowCounts(t *testing.T, file string) map[string]int {
	t.Helper()
	tables, err := column(file, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	counts := map[string]int{}
	for _, table := range tables {
		var n int
		if err = db.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
			t.Fatal(table, err)
		}
		counts[table] = n
	}
	return counts
}

func schemaVersion(t *testing.T, file string) int {
	t.Helper()
	v, err := column(file, "PRAGMA user_version")
	if err != nil || len(v) != 1 {
		t.Fatal("read schema version", v, err)
	}
	n, err := strconv.Atoi(v[0])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func column(file, query string) ([]string, error) {
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
