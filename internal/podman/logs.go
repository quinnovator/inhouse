package podman

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strconv"

	"github.com/quinnovator/inhouse/internal/store"
)

const (
	maxLogLines = 500
	maxLogBytes = 1 << 20
)

// Logs returns a container's newest output (stdout and stderr interleaved),
// at most tail lines and 1 MiB. It never follows.
func (p *Podman) Logs(ctx context.Context, r store.Revision, n string, tail int) (string, error) {
	if _, ok := r.Spec.Containers[n]; !ok {
		return "", errors.New("unknown container")
	}
	pod, err := p.inspectPod(ctx, r)
	if err != nil {
		return "", err
	}
	name := PodName(r) + "-" + n
	if _, err = p.inspectContainer(ctx, name, r, pod.ID); err != nil {
		return "", err
	}
	tail = min(max(tail, 1), maxLogLines)
	resp, err := p.request(ctx, "GET", "/containers/"+name+"/logs?stdout=true&stderr=true&tail="+strconv.Itoa(tail), nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return demuxNewest(resp.Body, maxLogBytes)
}

// demuxNewest strips Podman's 8-byte stream frame headers and keeps the
// newest limit bytes. Podman sends the oldest line first, so the end of the
// stream is what callers want.
func demuxNewest(body io.Reader, limit int) (string, error) {
	var out []byte
	keep := func(b []byte) {
		out = append(out, b...)
		if len(out) > 2*limit {
			out = append(out[:0:0], out[len(out)-limit:]...)
		}
	}
	header := make([]byte, 8)
	for {
		n, err := io.ReadFull(body, header)
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return "", err
		}
		if n < 8 || (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			// Not multiplexed: the rest is plain output.
			keep(header[:n])
			rest, err := io.ReadAll(body)
			keep(rest)
			if err != nil {
				return "", err
			}
			break
		}
		size := int64(binary.BigEndian.Uint32(header[4:8]))
		for size > 0 {
			chunk := make([]byte, min(size, 32<<10))
			m, err := io.ReadFull(body, chunk)
			keep(chunk[:m])
			size -= int64(m)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				size = 0 // a truncated final frame still yields its bytes
			} else if err != nil {
				return "", err
			}
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:] // drop the partial line at the cut
		}
	}
	return string(out), nil
}
