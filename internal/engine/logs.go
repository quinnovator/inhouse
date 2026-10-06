package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/store"
)

// LogWindow is the newest part of a container's log that callers can page through.
const LogWindow = 500

type LogQuery struct {
	Service   string
	Rev       int    // 0: the live revision, or the newest if none is live
	Container string // "": the ingress
	Tail      int    // lines per page, 1–500 (default 50)
	Cursor    string // from a previous page's next_cursor
}

type LogPage struct {
	Service   string   `json:"service"`
	Rev       int      `json:"rev"`
	Container string   `json:"container"`
	Lines     []string `json:"lines"`
	Next      string   `json:"next_cursor,omitempty"`
	Truncated bool     `json:"truncated"`
}

type logCursor struct {
	Service   string `json:"s"`
	Rev       int    `json:"r"`
	Container string `json:"c"`
	End       int    `json:"e"`
	Hash      string `json:"h"`
}

// Logs returns a page of a container's recent output with secret values
// redacted. Cursors page backward through the newest 500-line window; if the
// window moves (new output arrived), the cursor is refused rather than
// silently skipping or repeating lines.
func (e *Engine) Logs(ctx context.Context, q LogQuery) (LogPage, error) {
	if err := validService(q.Service); err != nil {
		return LogPage{}, err
	}
	if err := authz.From(ctx).RequireRead(q.Service); err != nil {
		return LogPage{}, err
	}
	if q.Tail == 0 {
		q.Tail = 50
	}
	if q.Tail < 1 || q.Tail > LogWindow {
		return LogPage{}, invalidf("tail must be 1–500")
	}
	r, err := e.logRevision(ctx, q.Service, q.Rev)
	if err != nil {
		return LogPage{}, err
	}
	if q.Container == "" {
		q.Container, _ = r.Spec.Ingress()
	}
	if _, ok := r.Spec.Containers[q.Container]; !ok {
		return LogPage{}, invalidf("r%d has no container %q", r.Rev, q.Container)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := e.runtime.Logs(bounded, r, q.Container, LogWindow)
	if err != nil {
		return LogPage{}, err
	}
	raw = e.vault.Redact(ctx, raw)
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	lines := []string{}
	if raw != "" {
		lines = strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	}
	end := len(lines)
	if q.Cursor != "" {
		c, ok := decodeCursor(q.Cursor)
		if !ok || c.Service != q.Service || c.Rev != r.Rev || c.Container != q.Container || c.End < 0 || c.End > len(lines) {
			return LogPage{}, invalidf("invalid log cursor")
		}
		if c.Hash != hash {
			return LogPage{}, invalidf("new log output arrived; request the newest page again without a cursor")
		}
		end = c.End
	}
	start := max(end-q.Tail, 0)
	page := LogPage{Service: q.Service, Rev: r.Rev, Container: q.Container, Lines: lines[start:end], Truncated: len(lines) >= LogWindow}
	if start > 0 {
		b, _ := json.Marshal(logCursor{q.Service, r.Rev, q.Container, start, hash})
		page.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

func decodeCursor(s string) (logCursor, bool) {
	var c logCursor
	if len(s) > 1024 {
		return c, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return c, err == nil && json.Unmarshal(b, &c) == nil
}

func (e *Engine) logRevision(ctx context.Context, service string, rev int) (store.Revision, error) {
	if rev > 0 {
		return e.store.Revision(ctx, service, rev)
	}
	svc, err := e.store.Service(ctx, service)
	if err != nil {
		return store.Revision{}, err
	}
	if svc.Current > 0 {
		return e.store.Revision(ctx, service, svc.Current)
	}
	revs, err := e.store.Revisions(ctx, service)
	if err != nil {
		return store.Revision{}, err
	}
	if len(revs) == 0 {
		return store.Revision{}, store.ErrNotFound
	}
	return revs[0], nil
}
