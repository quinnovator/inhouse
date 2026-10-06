package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// Secret values reach containers as Podman secrets scoped to one revision,
// <pod>-<secret>, created from the version the revision pinned and removed
// with the pod. Podman injects them as environment variables.

func secretName(r store.Revision, ref string) string { return PodName(r) + "-" + ref }

func (p *Podman) inspectSecret(ctx context.Context, r store.Revision, ref string) error {
	var info struct {
		Spec struct{ Labels map[string]string }
	}
	if err := p.call(ctx, "GET", "/secrets/"+secretName(r, ref)+"/json", nil, &info); err != nil {
		return err
	}
	if !owned(info.Spec.Labels, r) {
		return errors.New("podman secret is not labelled as this revision; refusing to use it")
	}
	return nil
}

func (p *Podman) ensureSecret(ctx context.Context, r store.Revision, ref, version string) (string, error) {
	if !spec.ValidName(ref) || len(version) != 64 {
		return "", errors.New("secret " + ref + " has no pinned version")
	}
	if p.Vault == nil {
		return "", errors.New("secret vault unavailable")
	}
	name := secretName(r, ref)
	err := p.inspectSecret(ctx, r, ref)
	if err == nil {
		return name, nil
	}
	if !isMissing(err) {
		return "", err
	}
	value, err := p.Vault.Version(ctx, version)
	if err != nil {
		return "", err
	}
	l, _ := json.Marshal(labels(r))
	query := url.Values{"name": {name}, "labels": {string(l)}}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://podman"+apiPrefix+"/secrets/create?"+query.Encode(), bytes.NewReader(value))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := p.do(req, "/secrets/create")
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	return name, p.inspectSecret(ctx, r, ref)
}

func (p *Podman) removeSecrets(ctx context.Context, r store.Revision) error {
	seen := map[string]bool{}
	for _, c := range r.Spec.Containers {
		for _, ref := range c.Secrets {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			err := p.inspectSecret(ctx, r, ref)
			if isMissing(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err = p.call(ctx, "DELETE", "/secrets/"+secretName(r, ref), nil, nil); err != nil && !isMissing(err) {
				return err
			}
		}
	}
	return nil
}
