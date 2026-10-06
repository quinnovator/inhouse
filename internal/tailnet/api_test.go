package tailnet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
