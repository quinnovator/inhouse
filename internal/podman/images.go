package podman

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/quinnovator/inhouse/internal/spec"
)

// Resolve pulls an image and returns the digest reference that pins it.
// A reference that is already pinned is pulled only if missing.
func (p *Podman) Resolve(ctx context.Context, image string) (string, error) {
	pinned := spec.Pinned(image)
	policy := "always"
	if pinned {
		policy = "missing"
	}
	if err := p.pull(ctx, image, policy); err != nil {
		return "", err
	}
	var info struct{ RepoDigests []string }
	if err := p.call(ctx, "GET", "/images/"+url.PathEscape(image)+"/json", nil, &info); err != nil {
		return "", err
	}
	if pinned {
		for _, d := range info.RepoDigests {
			if d == image {
				return d, nil
			}
		}
		return "", errors.New("pulled image does not match the requested digest")
	}
	repository := strings.Split(image, "@")[0]
	if i := strings.LastIndex(repository, ":"); i > strings.LastIndex(repository, "/") {
		repository = repository[:i]
	}
	// An image known under several names lists every digest it was ever
	// pulled by, including ones this registry never served (an upstream
	// multi-arch index, say). Pin only a digest the registry serves.
	for _, d := range info.RepoDigests {
		if spec.Pinned(d) && strings.HasPrefix(d, repository+"@") && p.pull(ctx, d, "always") == nil {
			return d, nil
		}
	}
	return "", errors.New("registry serves no digest for this image")
}

// pull pulls through Podman, which reports failures inside its progress
// stream rather than as an HTTP status.
func (p *Podman) pull(ctx context.Context, image, policy string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", "http://podman"+apiPrefix+"/images/pull?reference="+url.QueryEscape(image)+"&policy="+policy, nil)
	if err != nil {
		return err
	}
	if p.Vault != nil {
		host, _, _ := strings.Cut(image, "/")
		user, password, ok, err := p.Vault.RegistryAuth(ctx, host)
		if err != nil {
			return errors.New("registry credential unavailable")
		}
		if ok {
			// Credentials travel per request over the local socket, so no
			// plaintext auth file exists and they never leak to other hosts.
			raw, _ := json.Marshal(map[string]string{"username": user, "password": password, "serveraddress": host})
			req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(raw))
		}
	}
	resp, err := p.send(p.pulls, req, "/images/pull")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	for {
		var line struct {
			Error string `json:"error"`
		}
		err = dec.Decode(&line)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.New("invalid podman pull response")
		}
		if line.Error != "" {
			return errors.New("image pull failed: " + firstLine(line.Error))
		}
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
