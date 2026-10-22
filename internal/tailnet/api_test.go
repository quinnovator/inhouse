package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tailscale.com/tailcfg"
)

// fakeAPI serves the OAuth, key and device endpoints inhouse uses.
func fakeAPI(t *testing.T, device map[string]any) (*[]string, Credential) {
	t.Helper()
	var mu sync.Mutex
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/oauth/token":
			_ = r.ParseForm()
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token", "scope": r.Form.Get("scope")})
		case r.URL.Path == "/tailnet/-/keys":
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "tskey-auth-x"})
		case strings.HasPrefix(r.URL.Path, "/device/") && device == nil:
			http.NotFound(w, r)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(device)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	old := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = old })
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte("s3cret\n"), 0o600)
	return &calls, Credential{ClientID: "client", SecretFile: secret}
}

var id = Identity{DeviceID: "123", StableID: "nSTABLE", DNS: "blog.example.ts.net", Tag: TagService}

func TestMintKey(t *testing.T) {
	_, cred := fakeAPI(t, nil)
	key, err := MintKey(context.Background(), cred, TagService, false)
	if err != nil || key != "tskey-auth-x" {
		t.Fatal(key, err)
	}
	if _, err = MintKey(context.Background(), cred, "untagged", false); err == nil {
		t.Fatal("minted an untagged key")
	}
}

func TestDeleteDeviceChecksIdentity(t *testing.T) {
	calls, cred := fakeAPI(t, map[string]any{"id": "123", "nodeId": "nSTABLE", "name": "blog.example.ts.net.", "tags": []string{TagService}})
	stopped := false
	if err := DeleteDevice(context.Background(), cred, id, func() { stopped = true }); err != nil || !stopped {
		t.Fatal(err)
	}
	if last := (*calls)[len(*calls)-1]; last != "DELETE /device/123" {
		t.Fatal(*calls)
	}

	calls, cred = fakeAPI(t, map[string]any{"id": "123", "nodeId": "nSTABLE", "name": "blog.example.ts.net.", "tags": []string{TagService, "tag:other"}})
	if err := DeleteDevice(context.Background(), cred, id, nil); err == nil {
		t.Fatal("deleted a device whose tags changed")
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatal("issued DELETE for a mismatched device")
		}
	}

	_, cred = fakeAPI(t, nil)
	if err := DeleteDevice(context.Background(), cred, id, nil); err != nil {
		t.Fatal("a missing device should count as deleted", err)
	}
	control := id
	control.Tag = TagControl
	if err := DeleteDevice(context.Background(), cred, control, nil); err == nil {
		t.Fatal("deleted the control node")
	}
}

// TestDeviceIDIsDecimal guards the format the API looks devices up by.
// fmt.Sprint on a NodeID gives "nodeid:…", which the API finds no device
// for, and a missing device counts as deleted.
func TestDeviceIDIsDecimal(t *testing.T) {
	node := tailcfg.NodeID(5536624459523880)
	if got := DeviceID(node); got != "5536624459523880" {
		t.Fatalf("DeviceID = %q", got)
	}
	if fmt.Sprint(node) == DeviceID(node) {
		t.Fatal("NodeID's String no longer differs; this test no longer guards anything")
	}
}

func TestDeleteDeviceRefusesIDsTheAPIDoesNotKnow(t *testing.T) {
	calls, cred := fakeAPI(t, nil)
	old := id
	old.DeviceID = "nodeid:123"
	if err := DeleteDevice(context.Background(), cred, old, nil); err == nil {
		t.Fatal("a device ID the API can't find was treated as deleted")
	}
	if len(*calls) != 0 {
		t.Fatal("called the API with an unusable device ID", *calls)
	}
}

// TestOldIdentitiesDeleteTheirDevice reads an identity saved by 0.2.0 and
// deletes its device from an API that, like Tailscale's, knows the device
// only by its decimal ID.
func TestOldIdentitiesDeleteTheirDevice(t *testing.T) {
	dir := t.TempDir()
	raw := `{"device_id":"nodeid:123","stable_id":"nSTABLE","dns":"blog.example.ts.net","tag":"tag:inhouse-svc"}`
	if err := os.WriteFile(filepath.Join(dir, identityFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadIdentity(dir)
	if err != nil || saved.DeviceID != "123" {
		t.Fatalf("read %+v %v", saved, err)
	}
	var mu sync.Mutex
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			_ = r.ParseForm()
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token", "scope": r.Form.Get("scope")})
		case r.URL.Path != "/device/123":
			http.NotFound(w, r)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "123", "nodeId": "nSTABLE", "name": "blog.example.ts.net.", "tags": []string{TagService}})
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = true
			mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	base := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = base })
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte("s3cret\n"), 0o600)
	if err = DeleteDevice(context.Background(), Credential{ClientID: "client", SecretFile: secret}, saved, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleted {
		t.Fatal("the device was not deleted")
	}
}
