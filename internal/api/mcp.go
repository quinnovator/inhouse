package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/engine"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/version"
)

// MCPHandler serves stateless Streamable HTTP. Each request builds a server
// bound to that request's verified principal, so a session can never carry
// one caller's identity into another's call.
func MCPHandler(e *engine.Engine) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return NewMCPServer(e, authz.From(r.Context()))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 128 << 10})
	return http.NewCrossOriginProtection().Handler(h)
}

type (
	empty struct{}
	named struct {
		Name string `json:"name"`
	}
	deployQ struct {
		Spec json.RawMessage `json:"spec"`
		Key  string          `json:"idempotency_key,omitempty"`
	}
	waitQ struct {
		ID      string `json:"operation_id"`
		Timeout int    `json:"timeout_s,omitempty" jsonschema:"seconds to wait, 0–120 (default 60)"`
	}
	rollbackQ struct {
		Name    string `json:"name"`
		To      int    `json:"to_rev"`
		Restore bool   `json:"restore_volumes,omitempty" jsonschema:"admin only: also replace volume data with the target's snapshots"`
		Key     string `json:"idempotency_key,omitempty"`
	}
	logsQ struct {
		Name      string `json:"name"`
		Rev       int    `json:"rev,omitempty" jsonschema:"default: the live revision"`
		Container string `json:"container,omitempty" jsonschema:"default: the ingress container"`
		Tail      int    `json:"tail,omitempty" jsonschema:"lines per page, 1–500 (default 50)"`
		Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page, for older lines"`
	}
	eventsQ struct {
		Name  string `json:"name,omitempty"`
		Since int64  `json:"since_id,omitempty" jsonschema:"return events after this id, oldest first"`
		Limit int    `json:"limit,omitempty" jsonschema:"1–100 (default 20)"`
	}
	secretQ struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
)

// specBytes accepts a spec as a YAML string or a JSON object. The raw bytes
// are parsed directly, preserving container order.
func specBytes(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, &engine.Invalid{Err: errors.New("spec is required")}
	}
	if raw[0] == '"' {
		var yaml string
		if err := json.Unmarshal(raw, &yaml); err != nil {
			return nil, &engine.Invalid{Err: err}
		}
		return []byte(yaml), nil
	}
	return raw, nil
}

type toolKind int

const (
	readOnly toolKind = iota
	mutating
	destructive
)

func NewMCPServer(e *engine.Engine, p authz.Principal) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "inhouse", Version: version.Version}, nil)
	as := func(ctx context.Context) context.Context { return authz.With(ctx, p) }

	add(server, e, p, "whoami", "Return your verified tailnet identity and exact capability grants.", readOnly,
		func(ctx context.Context, _ empty) (any, error) { return e.Whoami(as(ctx)) })
	add(server, e, p, "list_services", "List services you can see, with URL, kind, live revision, health and expiry.", readOnly,
		func(ctx context.Context, _ empty) (any, error) { return e.List(as(ctx)) })
	add(server, e, p, "get_service", "Get one service: its health (healthy or degraded, with the reason and recent restarts), its ten newest revisions (with full pinned specs) and five newest events.", readOnly,
		func(ctx context.Context, in named) (any, error) { return e.Get(as(ctx), in.Name) })
	add(server, e, p, "plan_deploy", "Preview a deploy without changing anything: per-container images, env keys, secret names and volumes before and after, warnings, and whether you may apply it.", readOnly,
		func(ctx context.Context, in deployQ) (any, error) {
			raw, err := specBytes(in.Spec)
			if err != nil {
				return nil, err
			}
			return e.Plan(as(ctx), raw)
		})
	add(server, e, p, "deploy", "Deploy a stack spec. Returns an operation immediately; call wait_for_operation for the result. Traffic moves only after the new revision passes its health checks. Pass idempotency_key so a retry never deploys twice.", mutating,
		func(ctx context.Context, in deployQ) (any, error) {
			raw, err := specBytes(in.Spec)
			if err != nil {
				return nil, err
			}
			return e.Deploy(as(ctx), raw, in.Key)
		})
	add(server, e, p, "wait_for_operation", "Wait up to timeout_s (max 120) for an operation to finish; returns its state either way. A failed operation has a reason; get_events shows the failing container's last log lines.", readOnly,
		func(ctx context.Context, in waitQ) (any, error) {
			if in.Timeout == 0 {
				in.Timeout = 60
			}
			return e.Wait(as(ctx), in.ID, time.Duration(in.Timeout)*time.Second)
		})
	add(server, e, p, "rollback", "Deploy an exact copy of an earlier revision as a new revision. Volume data stays as it is unless restore_volumes is set (admin only), which REPLACES current volume data with the target's pre-deploy snapshots.", destructive,
		func(ctx context.Context, in rollbackQ) (any, error) {
			return e.Rollback(as(ctx), in.Name, in.To, in.Restore, in.Key)
		})
	add(server, e, p, "get_logs", "Read a container's recent output, newest page first, with secret values redacted. Use next_cursor for older lines within the newest 500.", readOnly,
		func(ctx context.Context, in logsQ) (any, error) {
			return e.Logs(as(ctx), engine.LogQuery{Service: in.Name, Rev: in.Rev, Container: in.Container, Tail: in.Tail, Cursor: in.Cursor})
		})
	add(server, e, p, "get_events", "Read the attributed event timeline (deploys, health failures with log tails, cutovers, live revisions degrading, restarting and recovering, deletes), optionally for one service.", readOnly,
		func(ctx context.Context, in eventsQ) (any, error) {
			return e.Events(as(ctx), store.EventQuery{Service: in.Name, Since: in.Since, Limit: in.Limit})
		})
	add(server, e, p, "delete_service", "DELETE a service: stops all its revisions, removes its tailnet node, and removes its volumes (persistent volumes are kept in trash for seven days). Deployers may delete only ephemeral services they created.", destructive,
		func(ctx context.Context, in named) (any, error) { return e.Delete(as(ctx), in.Name, "") })
	add(server, e, p, "list_secrets", "List secret names and when they changed. Values are never returned. Admin only.", readOnly,
		func(ctx context.Context, _ empty) (any, error) { return e.ListSecrets(as(ctx)) })
	add(server, e, p, "set_secret", "Encrypt and store a secret value, replacing any previous value; running revisions keep the value they were deployed with. The value is never returned. Admin only.", destructive,
		func(ctx context.Context, in secretQ) (any, error) {
			return map[string]any{"name": in.Name, "saved": true}, e.SetSecret(as(ctx), in.Name, in.Value)
		})
	add(server, e, p, "delete_secret", "Delete a secret no running revision uses. Admin only.", destructive,
		func(ctx context.Context, in named) (any, error) {
			return map[string]any{"name": in.Name, "deleted": true}, e.DeleteSecret(as(ctx), in.Name)
		})
	return server
}

func add[In any](server *mcp.Server, e *engine.Engine, p authz.Principal, name, description string, kind toolKind, handler func(context.Context, In) (any, error)) {
	closedWorld := false
	isDestructive := kind == destructive
	tool := &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint:    kind == readOnly,
		DestructiveHint: &isDestructive,
		IdempotentHint:  kind == readOnly,
		OpenWorldHint:   &closedWorld,
	}}
	if name == "deploy" || name == "plan_deploy" {
		tool.InputSchema = spec.DeployInputSchema()
	}
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		v, err := handler(ctx, in)
		if err != nil {
			if errors.Is(err, authz.ErrForbidden) {
				e.Audit(authz.With(ctx, p), "", "mcp "+name+": "+err.Error())
			}
			body, _ := json.Marshal(problem(err))
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
		}
		return nil, map[string]any{"result": v}, nil
	})
}
