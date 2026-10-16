package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quinnovator/inhouse/internal/version"
)

func TestWarnsOnceWhenTheDaemonVersionDiffers(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "1.2.0"
	daemon := "1.2.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(version.Header, daemon)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	call := func() string {
		var warn bytes.Buffer
		c := &client{base: srv.URL, http: srv.Client(), warn: &warn}
		for range 2 {
			var out any
			if err := c.call(context.Background(), "GET", "/v1/whoami", "", nil, &out); err != nil {
				t.Fatal(err)
			}
		}
		return warn.String()
	}
	if got := call(); got != "" {
		t.Fatal("warned about a matching daemon:", got)
	}
	daemon = "1.3.0"
	if got := call(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "1.3.0") {
		t.Fatalf("expected one warning naming the daemon's version, got %q", got)
	}
}
