// inhouse is the command-line client. It talks HTTPS to the control node
// over your tailnet; your tailnet identity is your credential.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/quinnovator/inhouse/internal/mcpbridge"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"github.com/quinnovator/inhouse/internal/version"
)

const usage = `inhouse — deploy and operate services on your inhouse host

Usage: inhouse [-url https://deploy.<tailnet>.ts.net] <command> [args]
The control URL can also come from INHOUSE_URL.

Services
  list                                   services you can see
  get SERVICE                            revisions and recent events
  plan SPEC                              preview a deploy; changes nothing
  deploy [-key K] [-no-wait] SPEC        deploy a stack spec (YAML or JSON)
  rollback [-key K] [-restore-volumes] SERVICE REV
                                         redeploy an earlier revision
  delete [-key K] SERVICE                delete a service and its node
  wait OPERATION                         wait for an operation to finish
  logs [-rev N] [-container C] [-tail N] SERVICE
  events [-since ID] [-limit N] [SERVICE]

Secrets (admin)
  secrets list
  secrets set NAME < value               reads the value from stdin
  secrets delete NAME

Other
  whoami                                 your identity and grants
  schema                                 print the stack spec JSON Schema
  mcp-stdio                              MCP over stdio, as this device
  mcp-agent [-tag T] [-client-id ID] [-secret-file F]
                                         MCP over stdio, as a scoped agent node
  version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "inhouse:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	global := flag.NewFlagSet("inhouse", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	base := global.String("url", os.Getenv("INHOUSE_URL"), "control node URL")
	if err := global.Parse(args); err != nil {
		return err
	}
	args = global.Args()
	if len(args) == 0 {
		global.Usage()
		return errors.New("no command")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Println(version.Version)
		return nil
	case "schema":
		_, err := os.Stdout.Write(spec.Schema())
		return err
	case "help", "-h", "--help":
		global.Usage()
		return nil
	}
	if err := checkURL(*base); err != nil {
		return err
	}
	c := &client{base: strings.TrimRight(*base, "/"), http: &http.Client{
		Timeout:       130 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	switch cmd {
	case "whoami":
		return c.show(ctx, "GET", "/v1/whoami", noArgs(args))
	case "list":
		return c.show(ctx, "GET", "/v1/services", noArgs(args))
	case "get":
		name, err := oneService(args)
		return c.show(ctx, "GET", "/v1/services/"+name, err)
	case "plan":
		if len(args) != 1 {
			return errors.New("usage: plan SPEC")
		}
		body, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		var out any
		if err = c.call(ctx, "POST", "/v1/plan", "", body, &out); err != nil {
			return err
		}
		return printJSON(out)
	case "deploy":
		return c.deploy(ctx, args)
	case "rollback":
		return c.rollback(ctx, args)
	case "delete":
		fs := flag.NewFlagSet("delete", flag.ContinueOnError)
		key := fs.String("key", uuid.NewString(), "idempotency key")
		if err := fs.Parse(args); err != nil {
			return err
		}
		name, err := oneService(fs.Args())
		if err != nil {
			return err
		}
		return c.operate(ctx, "DELETE", "/v1/services/"+name, *key, nil, true)
	case "wait":
		if len(args) != 1 {
			return errors.New("usage: wait OPERATION")
		}
		op, err := c.wait(ctx, args[0])
		if err != nil {
			return err
		}
		return c.finish(ctx, op)
	case "logs":
		return c.logs(ctx, args)
	case "events":
		fs := flag.NewFlagSet("events", flag.ContinueOnError)
		since := fs.Int("since", 0, "only events after this id")
		limit := fs.Int("limit", 20, "number of events, 1–100")
		if err := fs.Parse(args); err != nil {
			return err
		}
		q := url.Values{"since_id": {strconv.Itoa(*since)}, "limit": {strconv.Itoa(*limit)}}
		if fs.NArg() > 1 {
			return errors.New("usage: events [SERVICE]")
		}
		if fs.NArg() == 1 {
			q.Set("service", fs.Arg(0))
		}
		var events []struct {
			ID      int64  `json:"id"`
			TS      int64  `json:"ts"`
			Service string `json:"service"`
			Rev     int    `json:"rev"`
			Actor   string `json:"actor"`
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}
		if err := c.call(ctx, "GET", "/v1/events?"+q.Encode(), "", nil, &events); err != nil {
			return err
		}
		for _, v := range events {
			where := v.Service
			if v.Rev > 0 {
				where += "/r" + strconv.Itoa(v.Rev)
			}
			fmt.Printf("%d  %s  %-20s %-16s %-24s %s\n", v.ID, time.Unix(v.TS, 0).Format(time.DateTime), where, v.Kind, v.Actor, strings.ReplaceAll(v.Message, "\n", "\n    "))
		}
		return nil
	case "secrets":
		return c.secrets(ctx, args)
	case "mcp-stdio":
		if err := noArgs(args); err != nil {
			return err
		}
		return mcpbridge.RunAsDevice(ctx, c.base)
	case "mcp-agent":
		fs := flag.NewFlagSet("mcp-agent", flag.ContinueOnError)
		tag := fs.String("tag", tailnet.TagAgent, "tag the agent node joins with")
		id := fs.String("client-id", "", "agent OAuth client ID (default: ~/.config/inhouse/agent/client-id)")
		secret := fs.String("secret-file", "", "agent OAuth client secret file (default: ~/.config/inhouse/agent/client-secret)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		cred, err := mcpbridge.DefaultAgentCredential()
		if *id != "" && *secret != "" {
			cred, err = tailnet.Credential{ClientID: *id, SecretFile: *secret}, nil
		}
		if err != nil {
			return err
		}
		return mcpbridge.RunAsAgent(ctx, c.base, mcpbridge.Agent{Tag: *tag, Credential: cred})
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func checkURL(raw string) error {
	if raw == "" {
		return errors.New("set -url or INHOUSE_URL to the control node, e.g. https://deploy.<tailnet>.ts.net")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("the control URL must be a plain https:// origin")
	}
	return nil
}

func noArgs(args []string) error {
	if len(args) != 0 {
		return errors.New("unexpected arguments")
	}
	return nil
}

func oneService(args []string) (string, error) {
	if len(args) != 1 || !spec.ValidName(args[0]) {
		return "", errors.New("expected one service name")
	}
	return args[0], nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type client struct {
	base string
	http *http.Client
}

type operation struct {
	ID      string `json:"operation_id"`
	Kind    string `json:"kind"`
	Service string `json:"service"`
	Rev     int    `json:"rev"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
}

func (c *client) call(ctx context.Context, method, path, key string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var p struct {
			Error struct {
				Code, Message, Hint string
				OperationID         string `json:"operation_id"`
			}
		}
		if json.Unmarshal(raw, &p) != nil || p.Error.Message == "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		msg := p.Error.Code + ": " + p.Error.Message
		if p.Error.OperationID != "" {
			msg += " (wait with: inhouse wait " + p.Error.OperationID + ")"
		}
		return errors.New(msg)
	}
	return json.Unmarshal(raw, out)
}

func (c *client) show(ctx context.Context, method, path string, argErr error) error {
	if argErr != nil {
		return argErr
	}
	var out any
	if err := c.call(ctx, method, path, "", nil, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func (c *client) deploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	key := fs.String("key", uuid.NewString(), "idempotency key; reuse it to retry safely")
	noWait := fs.Bool("no-wait", false, "return once the deploy is recorded")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: deploy [-key K] [-no-wait] SPEC")
	}
	body, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	if _, err = spec.Parse(body); err != nil {
		return err
	}
	return c.operate(ctx, "POST", "/v1/deploy", *key, body, !*noWait)
}

func (c *client) rollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	key := fs.String("key", uuid.NewString(), "idempotency key; reuse it to retry safely")
	restore := fs.Bool("restore-volumes", false, "admin: also replace volume data with the target's snapshots (downtime, overwrites data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || !spec.ValidName(fs.Arg(0)) {
		return errors.New("usage: rollback [-key K] [-restore-volumes] SERVICE REV")
	}
	rev, err := strconv.Atoi(strings.TrimPrefix(fs.Arg(1), "r"))
	if err != nil || rev < 1 {
		return errors.New("REV must be a revision number such as 3 or r3")
	}
	body, _ := json.Marshal(map[string]any{"to_rev": rev, "restore_volumes": *restore})
	return c.operate(ctx, "POST", "/v1/services/"+fs.Arg(0)+"/rollback", *key, body, true)
}

// operate starts an operation and, if asked, waits for it to finish.
func (c *client) operate(ctx context.Context, method, path, key string, body []byte, wait bool) error {
	var op operation
	if err := c.call(ctx, method, path, key, body, &op); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s %s: operation %s (idempotency key %s)\n", op.Kind, op.Service, op.ID, key)
	if !wait {
		return printJSON(op)
	}
	op, err := c.wait(ctx, op.ID)
	if err != nil {
		return err
	}
	return c.finish(ctx, op)
}

func (c *client) wait(ctx context.Context, id string) (operation, error) {
	for {
		var op operation
		if err := c.call(ctx, "GET", "/v1/operations/"+url.PathEscape(id)+"?wait=120", "", nil, &op); err != nil {
			return op, err
		}
		if op.State != "running" {
			return op, nil
		}
		fmt.Fprintln(os.Stderr, "still running…")
	}
}

// finish prints the outcome and the service URL, failing on a failed operation.
func (c *client) finish(ctx context.Context, op operation) error {
	out := map[string]any{"operation": op}
	if op.State == "succeeded" && op.Kind != "delete" {
		var detail struct {
			Service struct {
				URL string `json:"url"`
			} `json:"service"`
		}
		if err := c.call(ctx, "GET", "/v1/services/"+op.Service, "", nil, &detail); err == nil {
			out["url"] = detail.Service.URL
		}
	}
	if err := printJSON(out); err != nil {
		return err
	}
	if op.State == "failed" {
		return fmt.Errorf("%s failed: %s (see: inhouse events %s)", op.Kind, op.Reason, op.Service)
	}
	return nil
}

func (c *client) logs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	rev := fs.Int("rev", 0, "revision (default: live)")
	container := fs.String("container", "", "container (default: the ingress)")
	tail := fs.Int("tail", 100, "lines, 1–500")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := oneService(fs.Args())
	if err != nil {
		return err
	}
	q := url.Values{"rev": {strconv.Itoa(*rev)}, "container": {*container}, "tail": {strconv.Itoa(*tail)}}
	var page struct {
		Lines []string `json:"lines"`
	}
	if err = c.call(ctx, "GET", "/v1/services/"+name+"/logs?"+q.Encode(), "", nil, &page); err != nil {
		return err
	}
	for _, line := range page.Lines {
		fmt.Println(line)
	}
	return nil
}

func (c *client) secrets(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: secrets list | set NAME | delete NAME")
	}
	switch {
	case args[0] == "list" && len(args) == 1:
		return c.show(ctx, "GET", "/v1/secrets", nil)
	case args[0] == "set" && len(args) == 2:
		value, err := io.ReadAll(io.LimitReader(os.Stdin, 32<<10+1))
		if err != nil {
			return err
		}
		value = bytes.TrimSuffix(value, []byte("\n"))
		body, _ := json.Marshal(map[string]string{"value": string(value)})
		var out any
		if err = c.call(ctx, "PUT", "/v1/secrets/"+args[1], "", body, &out); err != nil {
			return err
		}
		return printJSON(out)
	case args[0] == "delete" && len(args) == 2:
		return c.show(ctx, "DELETE", "/v1/secrets/"+args[1], nil)
	}
	return errors.New("usage: secrets list | set NAME | delete NAME")
}
