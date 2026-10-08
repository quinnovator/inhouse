package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

func open(t *testing.T) (*Vault, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	key := filepath.Join(dir, "keys", "age.key")
	v, err := Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	return v, db, key
}

func TestKeyFileIsPrivateAndReused(t *testing.T) {
	v, db, key := open(t)
	if info, _ := os.Stat(key); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if err := v.Set(context.Background(), "token", "s3cret", "me"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	if out := again.Redact(context.Background(), "token=s3cret"); out != "token=[REDACTED]" {
		t.Fatal(out)
	}
	_ = os.Chmod(key, 0o644)
	if _, err = Open(db, key); err == nil {
		t.Fatal("accepted a world-readable key")
	}
}

func TestCiphertextNeverContainsValue(t *testing.T) {
	v, db, _ := open(t)
	ctx := context.Background()
	_ = v.Set(ctx, "token", "plain-value", "me")
	c, _ := db.Secret(ctx, "token")
	if strings.Contains(string(c), "plain-value") {
		t.Fatal("stored in plaintext")
	}
}

func TestPinKeepsOldVersions(t *testing.T) {
	v, _, _ := open(t)
	ctx := context.Background()
	_ = v.Set(ctx, "db", "first", "me")
	s := spec.Stack{Containers: map[string]spec.Container{"web": {Secrets: map[string]string{"PW": "db"}}}}
	if err := v.Pin(ctx, &s); err != nil {
		t.Fatal(err)
	}
	_ = v.Set(ctx, "db", "second", "me")
	got, err := v.Version(ctx, s.Containers["web"].SecretVersions["db"])
	if err != nil || string(got) != "first" {
		t.Fatal(string(got), err)
	}
	if out := v.Redact(ctx, "first second"); out != "[REDACTED] [REDACTED]" {
		t.Fatal(out)
	}
	missing := spec.Stack{Containers: map[string]spec.Container{"web": {Secrets: map[string]string{"PW": "nope"}}}}
	if err = v.Pin(ctx, &missing); err == nil {
		t.Fatal("pinned a missing secret")
	}
}

func TestRegistryCredentials(t *testing.T) {
	v, _, _ := open(t)
	ctx := context.Background()
	if err := v.Set(ctx, "registry-auth/ghcr.io", "not json", "me"); err == nil {
		t.Fatal("accepted malformed registry credential")
	}
	if err := v.Set(ctx, "registry-auth/ghcr.io", `{"username":"u","password":"tok"}`, "me"); err != nil {
		t.Fatal(err)
	}
	user, pw, ok, err := v.RegistryAuth(ctx, "ghcr.io")
	if err != nil || !ok || user != "u" || pw != "tok" {
		t.Fatal(user, pw, ok, err)
	}
	if _, _, ok, _ = v.RegistryAuth(ctx, "docker.io"); ok {
		t.Fatal("credential leaked to another host")
	}
	if out := v.Redact(ctx, "auth tok"); out != "auth [REDACTED]" {
		t.Fatal(out)
	}
	for _, bad := range []string{"registry-auth/", "registry-auth/a b", "registry-auth/u@h", "registry-auth/h/path", "UPPER"} {
		if ValidName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// bothPaths runs a test with the cached matcher and again past its budget.
func bothPaths(t *testing.T, f func(t *testing.T)) {
	t.Run("matcher", f)
	t.Run("each", func(t *testing.T) {
		old := matcherBudget
		matcherBudget = 0
		t.Cleanup(func() { matcherBudget = old })
		f(t)
	})
}

func TestRedactNestedSecretsLeaveNothing(t *testing.T) { bothPaths(t, testRedactNested) }

func testRedactNested(t *testing.T) {
	v, _, _ := open(t)
	ctx := context.Background()
	// Several pairs, so no storage order happens to redact them all.
	var in, want []string
	for i := range 8 {
		short := fmt.Sprintf("key%d", i)
		_ = v.Set(ctx, fmt.Sprintf("short%d", i), short, "me")
		_ = v.Set(ctx, fmt.Sprintf("long%d", i), short+"-tail", "me")
		in = append(in, short+"-tail")
		want = append(want, "[REDACTED]")
	}
	if out := v.Redact(ctx, strings.Join(in, " ")); out != strings.Join(want, " ") {
		t.Fatal(out)
	}
}

func TestRedactSeesNewAndChangedSecrets(t *testing.T) { bothPaths(t, testRedactChanges) }

func testRedactChanges(t *testing.T) {
	v, _, _ := open(t)
	ctx := context.Background()
	_ = v.Set(ctx, "a", "first", "me")
	if out := v.Redact(ctx, "first later"); out != "[REDACTED] later" {
		t.Fatal(out)
	}
	_ = v.Set(ctx, "b", "later", "me")
	if out := v.Redact(ctx, "first later"); out != "[REDACTED] [REDACTED]" {
		t.Fatal(out)
	}
	// Replacing an unpinned value drops the old one from the set.
	_ = v.Set(ctx, "a", "again", "me")
	if out := v.Redact(ctx, "first again"); out != "first [REDACTED]" {
		t.Fatal(out)
	}
}

func TestRedactPastBudgetKeepsNoMatcher(t *testing.T) {
	v, _, _ := open(t)
	ctx := context.Background()
	_ = v.Set(ctx, "a", "first", "me")
	_ = v.Redact(ctx, "warm the cache")
	if v.redactor == nil {
		t.Fatal("no matcher under budget")
	}
	old := matcherBudget
	matcherBudget = 0
	t.Cleanup(func() { matcherBudget = old })
	if out := v.Redact(ctx, "first"); out != "[REDACTED]" {
		t.Fatal(out)
	}
	if v.redactor != nil || v.plain != nil {
		t.Fatal("kept values past budget")
	}
}
