package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBase is the Tailscale API. Error messages never include response
// bodies or credentials.
var APIBase = "https://api.tailscale.com/api/v2"

var httpClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func exchange(req *http.Request, out any) (int, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, errors.New("tailscale API connection failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("tailscale API %s returned HTTP %d", req.URL.Path, resp.StatusCode)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return resp.StatusCode, errors.New("invalid tailscale API response")
	}
	return resp.StatusCode, nil
}

// Token exchanges OAuth client credentials for a short-lived access token
// limited to scope (and to tags, when given).
func Token(ctx context.Context, c Credential, scope string, tags []string) (string, error) {
	secret, err := c.secret()
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {c.ClientID}, "client_secret": {secret}, "grant_type": {"client_credentials"}, "scope": {scope}}
	if len(tags) > 0 {
		form.Set("tags", strings.Join(tags, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var result struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if _, err = exchange(req, &result); err != nil {
		return "", err
	}
	if result.AccessToken == "" || result.Scope != scope {
		return "", errors.New("OAuth token response has the wrong scope")
	}
	return result.AccessToken, nil
}

// MintKey creates a single-use, pre-authorized auth key for one tag that
// expires in five minutes.
func MintKey(ctx context.Context, c Credential, tag string, ephemeral bool) (string, error) {
	if !strings.HasPrefix(tag, "tag:") {
		return "", errors.New("auth keys are minted only for tagged nodes")
	}
	token, err := Token(ctx, c, "auth_keys", []string{tag})
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"expirySeconds": 300,
		"description":   "inhouse node enrollment",
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": ephemeral, "preauthorized": true, "tags": []string{tag},
		}}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+"/tailnet/-/keys", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	var result struct {
		Key string `json:"key"`
	}
	if _, err = exchange(req, &result); err != nil {
		return "", err
	}
	if result.Key == "" {
		return "", errors.New("empty auth key response")
	}
	return result.Key, nil
}

// DeleteDevice deletes the device described by id, but only after the API
// confirms the device still has that exact ID, node ID, name and sole tag.
// The lifecycle credential can manage any device in the tailnet, so these
// checks are what confine it to inhouse's own nodes. A missing device counts
// as deleted.
func DeleteDevice(ctx context.Context, c Credential, id Identity, beforeDelete func()) error {
	if id.DeviceID == "" || id.StableID == "" || id.DNS == "" || (id.Tag != TagService && id.Tag != TagEphemeral) {
		return errors.New("saved node identity is incomplete; refusing device deletion")
	}
	token, err := Token(ctx, c, "devices:core", []string{TagService, TagEphemeral})
	if err != nil {
		return err
	}
	call := func(method string, out any) (int, error) {
		req, err := http.NewRequestWithContext(ctx, method, APIBase+"/device/"+url.PathEscape(id.DeviceID), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return exchange(req, out)
	}
	var device struct {
		ID     string   `json:"id"`
		NodeID string   `json:"nodeId"`
		Name   string   `json:"name"`
		Tags   []string `json:"tags"`
	}
	status, err := call(http.MethodGet, &device)
	exists := status != http.StatusNotFound
	if exists && err != nil {
		return err
	}
	if exists && (device.ID != id.DeviceID || device.NodeID != id.StableID || strings.TrimSuffix(device.Name, ".") != id.DNS || len(device.Tags) != 1 || device.Tags[0] != id.Tag) {
		return errors.New("device identity or tags changed since inhouse created it; refusing deletion")
	}
	if beforeDelete != nil {
		beforeDelete()
	}
	if !exists {
		return nil
	}
	// Logging out an ephemeral node may already have removed it.
	if status, err = call(http.MethodDelete, nil); err != nil && status != http.StatusNotFound {
		return err
	}
	return nil
}
