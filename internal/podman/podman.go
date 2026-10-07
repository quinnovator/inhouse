// Package podman runs revisions as rootless Podman pods through Podman's REST
// API on a Unix socket.
//
// Each revision is one pod, svc-<service>-r<rev>, whose containers share a
// network namespace. Only the ingress port is published, on 127.0.0.1. Every
// pod, container and Podman secret carries inhouse labels, and every mutating
// call first verifies them: objects inhouse didn't create are never touched.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/vault"
)

const apiPrefix = "/v4.0.0/libpod"

const (
	LabelOwner    = "inhouse"
	LabelService  = "inhouse.service"
	LabelRevision = "inhouse.revision"
	LabelSpec     = "inhouse.spec"
)

// HTTPError is a non-2xx answer from Podman.
type HTTPError struct {
	Status int
	Path   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("podman %s: HTTP %d", e.Path, e.Status) }

func isMissing(err error) bool {
	var h *HTTPError
	return errors.As(err, &h) && h.Status == http.StatusNotFound
}

// VolumePaths prepares a service's volume directory for mounting.
type VolumePaths interface {
	Ensure(ctx context.Context, service, volume string) (string, error)
}

type Podman struct {
	client *http.Client
	// Offset returns where the service's container ID 0 maps (see userns).
	Offset func(ctx context.Context, service string) (int, error)
	// Network is the pod network mode: pasta (default) or slirp4netns.
	Network string
	Volumes VolumePaths
	Vault   *vault.Vault
}

// New talks to the Podman API listening on socket.
func New(socket string) *Podman {
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Podman{client: &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (p *Podman) Close() { p.client.CloseIdleConnections() }

func (p *Podman) do(req *http.Request, path string) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, errors.New("podman API unavailable")
	}
	if resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, &HTTPError{resp.StatusCode, path}
	}
	return resp, nil
}

func (p *Podman) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://podman"+apiPrefix+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req, strings.Split(path, "?")[0])
}

func (p *Podman) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := p.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// PodName names a revision's pod; containers are <pod>-<container>.
func PodName(r store.Revision) string { return fmt.Sprintf("svc-%s-r%d", r.Service, r.Rev) }

func labels(r store.Revision) map[string]string {
	return map[string]string{LabelOwner: "1", LabelService: r.Service, LabelRevision: strconv.Itoa(r.Rev), LabelSpec: r.Hash}
}

func owned(got map[string]string, r store.Revision) bool {
	for k, v := range labels(r) {
		if got[k] != v {
			return false
		}
	}
	return true
}

type podInfo struct {
	ID               string `json:"Id"`
	Labels           map[string]string
	InfraContainerID string
	Containers       []struct {
		ID   string `json:"Id"`
		Name string
	}
}

func (p *Podman) inspectPod(ctx context.Context, r store.Revision) (podInfo, error) {
	var pod podInfo
	err := p.call(ctx, "GET", "/pods/"+PodName(r)+"/json", nil, &pod)
	if err == nil && !owned(pod.Labels, r) {
		err = fmt.Errorf("pod %s is not labelled as this revision; refusing to touch it", PodName(r))
	}
	return pod, err
}

type containerInfo struct {
	Pod    string
	Config struct{ Labels map[string]string }
	State  struct{ Running bool }
}

func (p *Podman) inspectContainer(ctx context.Context, name string, r store.Revision, pod string) (containerInfo, error) {
	var c containerInfo
	err := p.call(ctx, "GET", "/containers/"+name+"/json", nil, &c)
	if err == nil && (!owned(c.Config.Labels, r) || c.Pod != pod) {
		err = fmt.Errorf("container %s is not labelled as this revision; refusing to touch it", name)
	}
	return c, err
}

// verifyMembers checks every container in the pod belongs to the revision.
func (p *Podman) verifyMembers(ctx context.Context, pod podInfo, r store.Revision) error {
	allowed := map[string]bool{pod.InfraContainerID: true}
	for n := range r.Spec.Containers {
		allowed[PodName(r)+"-"+n] = true
	}
	for _, c := range pod.Containers {
		if !allowed[c.ID] && !allowed[c.Name] {
			return fmt.Errorf("pod %s has unexpected member %s; refusing to touch it", PodName(r), c.Name)
		}
		if c.ID == pod.InfraContainerID {
			continue
		}
		if _, err := p.inspectContainer(ctx, c.ID, r, pod.ID); err != nil {
			return err
		}
	}
	return nil
}

// Up creates whatever part of the revision's pod is missing and starts every
// container, sidecars first and the ingress last.
func (p *Podman) Up(ctx context.Context, r store.Revision) error {
	offset, err := p.Offset(ctx, r.Service)
	if err != nil {
		return err
	}
	pod, err := p.inspectPod(ctx, r)
	if isMissing(err) {
		if err = p.createPod(ctx, r, offset); err != nil {
			return err
		}
		pod, err = p.inspectPod(ctx, r)
	}
	if err != nil {
		return err
	}
	if err = p.verifyMembers(ctx, pod, r); err != nil {
		return err
	}
	for _, n := range r.Spec.StartOrder() {
		name := PodName(r) + "-" + n
		info, err := p.inspectContainer(ctx, name, r, pod.ID)
		if isMissing(err) {
			if err = p.createContainer(ctx, r, n, pod.ID); err != nil {
				return err
			}
			info, err = p.inspectContainer(ctx, name, r, pod.ID)
		}
		if err != nil {
			return err
		}
		if !info.State.Running {
			if err = p.call(ctx, "POST", "/containers/"+name+"/start", nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Podman) createPod(ctx context.Context, r store.Revision, offset int) error {
	_, ingress := r.Spec.Ingress()
	network := p.Network
	if network == "" {
		network = "pasta"
	}
	ids := []any{map[string]int{"ContainerID": 0, "HostID": offset, "Size": 65536}}
	return p.call(ctx, "POST", "/pods/create", map[string]any{
		"name":              PodName(r),
		"hostname":          PodName(r),
		"labels":            labels(r),
		"userns":            map[string]string{"nsmode": "private"},
		"idmappings":        map[string]any{"HostUIDMapping": false, "HostGIDMapping": false, "UIDMap": ids, "GIDMap": ids},
		"netns":             map[string]string{"nsmode": network},
		"shared_namespaces": []string{"ipc", "net", "uts"},
		"portmappings": []any{map[string]any{
			"host_ip": "127.0.0.1", "host_port": r.Port, "container_port": ingress.Port, "protocol": "tcp", "range": 1,
		}},
	}, nil)
}

func (p *Podman) createContainer(ctx context.Context, r store.Revision, n, pod string) error {
	c := r.Spec.Containers[n]
	memory, _ := spec.Memory(c.Resources.Memory)
	body := map[string]any{
		"name":           PodName(r) + "-" + n,
		"pod":            pod,
		"image":          c.Image,
		"labels":         labels(r),
		"restart_policy": "on-failure",
		"restart_tries":  3,
		"env":            c.Env,
		"env_host":       false,
		"httpproxy":      false,
		"resource_limits": map[string]any{
			"memory": map[string]any{"limit": memory},
			"cpu":    map[string]any{"period": 100000, "quota": int64(c.Resources.CPUs * 100000)},
		},
	}
	if len(c.Command) > 0 {
		body["entrypoint"] = c.Command
	}
	if len(c.Args) > 0 {
		body["command"] = c.Args
	}
	if len(c.Secrets) > 0 {
		env := map[string]string{}
		for target, ref := range c.Secrets {
			name, err := p.ensureSecret(ctx, r, ref, c.SecretVersions[ref])
			if err != nil {
				return err
			}
			env[target] = name
		}
		body["secret_env"] = env
	}
	if len(c.Volumes) > 0 {
		mounts := []any{}
		for volume, target := range c.Volumes {
			src, err := p.Volumes.Ensure(ctx, r.Service, volume)
			if err != nil {
				return err
			}
			mounts = append(mounts, map[string]any{"source": src, "destination": target, "type": "bind", "options": []string{"rw", "rbind", "z"}})
		}
		body["mounts"] = mounts
		// Volume ACLs grant the service's mapped GID 0; any image user gets
		// access through this supplementary group.
		body["groups"] = []string{"0"}
	}
	return p.call(ctx, "POST", "/containers/create", body, nil)
}

// Running reports whether the pod exists and every container is running.
func (p *Podman) Running(ctx context.Context, r store.Revision) (bool, error) {
	pod, err := p.inspectPod(ctx, r)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = p.verifyMembers(ctx, pod, r); err != nil {
		return false, err
	}
	for n := range r.Spec.Containers {
		info, err := p.inspectContainer(ctx, PodName(r)+"-"+n, r, pod.ID)
		if isMissing(err) {
			return false, nil
		}
		if err != nil || !info.State.Running {
			return false, err
		}
	}
	return true, nil
}

func (p *Podman) Stop(ctx context.Context, r store.Revision) error {
	pod, err := p.inspectPod(ctx, r)
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = p.verifyMembers(ctx, pod, r); err != nil {
		return err
	}
	err = p.call(ctx, "POST", "/pods/"+PodName(r)+"/stop?t=5", nil, nil)
	var h *HTTPError
	if errors.As(err, &h) && h.Status == http.StatusNotModified {
		return nil // already stopped
	}
	return err
}

func (p *Podman) Remove(ctx context.Context, r store.Revision) error {
	pod, err := p.inspectPod(ctx, r)
	if isMissing(err) {
		return p.removeSecrets(ctx, r)
	}
	if err != nil {
		return err
	}
	if err = p.verifyMembers(ctx, pod, r); err != nil {
		return err
	}
	if err = p.call(ctx, "DELETE", "/pods/"+PodName(r)+"?force=true", nil, nil); err != nil && !isMissing(err) {
		return err
	}
	return p.removeSecrets(ctx, r)
}

// Check runs one health check: the container's command, an HTTP GET for an
// ingress with a path, a TCP connect for an ingress without one, or just
// "still running" for a sidecar without a check.
func (p *Podman) Check(ctx context.Context, r store.Revision, n string) error {
	c := r.Spec.Containers[n]
	pod, err := p.inspectPod(ctx, r)
	if err != nil {
		return err
	}
	info, err := p.inspectContainer(ctx, PodName(r)+"-"+n, r, pod.ID)
	if err != nil {
		return err
	}
	if !info.State.Running {
		return errors.New("container is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if len(c.Health.Command) > 0 {
		return p.exec(ctx, PodName(r)+"-"+n, c.Health.Command)
	}
	if c.Port == 0 {
		return nil
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Port))
	if c.Health.Path == "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return errors.New("TCP connect failed")
		}
		return conn.Close()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+address+c.Health.Path, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("HTTP request failed")
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, c.Health.Path)
	}
	return nil
}

func (p *Podman) exec(ctx context.Context, container string, command []string) error {
	var created struct{ ID string }
	if err := p.call(ctx, "POST", "/containers/"+container+"/exec", map[string]any{"Cmd": command, "AttachStdout": false, "AttachStderr": false, "Tty": false}, &created); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = p.call(cleanup, "POST", "/exec/"+created.ID+"/remove?force=true", nil, nil)
	}()
	if err := p.call(ctx, "POST", "/exec/"+created.ID+"/start", map[string]any{"Detach": true, "Tty": false}, nil); err != nil {
		return err
	}
	for {
		var info struct {
			Running  bool
			ExitCode int
		}
		if err := p.call(ctx, "GET", "/exec/"+created.ID+"/json", nil, &info); err != nil {
			return err
		}
		if !info.Running {
			if info.ExitCode != 0 {
				return fmt.Errorf("health command exited %d", info.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("health command timed out")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Pods lists labelled pods as the revisions their labels claim.
func (p *Podman) Pods(ctx context.Context) ([]store.Revision, error) {
	var pods []struct {
		Name    string
		Labels  map[string]string
		Created time.Time
	}
	filters := url.QueryEscape(`{"label":["` + LabelOwner + `=1"]}`)
	if err := p.call(ctx, "GET", "/pods/json?filters="+filters, nil, &pods); err != nil {
		return nil, err
	}
	out := []store.Revision{}
	for _, pod := range pods {
		r := store.Revision{Service: pod.Labels[LabelService], Hash: pod.Labels[LabelSpec], Created: pod.Created.Unix()}
		var err error
		r.Rev, err = strconv.Atoi(pod.Labels[LabelRevision])
		if err != nil || r.Rev < 1 || !spec.ValidName(r.Service) || r.Hash == "" || pod.Name != PodName(r) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// RemoveOrphan force-removes a labelled pod after re-verifying its labels,
// then the secrets labelled for the same revision.
func (p *Podman) RemoveOrphan(ctx context.Context, r store.Revision) error {
	pod, err := p.inspectPod(ctx, r)
	if isMissing(err) {
		return p.removeLabelled(ctx, r.Service, func(l map[string]string) bool { return owned(l, r) })
	}
	if err != nil {
		return err
	}
	for _, c := range pod.Containers {
		if c.ID == pod.InfraContainerID {
			continue
		}
		if !strings.HasPrefix(c.Name, PodName(r)+"-") {
			return fmt.Errorf("orphan pod %s has unexpected member %s", PodName(r), c.Name)
		}
		if _, err = p.inspectContainer(ctx, c.ID, r, pod.ID); err != nil {
			return err
		}
	}
	if err = p.call(ctx, "DELETE", "/pods/"+PodName(r)+"?force=true", nil, nil); err != nil && !isMissing(err) {
		return err
	}
	return p.removeLabelled(ctx, r.Service, func(l map[string]string) bool { return owned(l, r) })
}
