// Package mcpbridge serves the control node's remote MCP tools over stdio,
// for agents that only launch local MCP servers.
//
// As the device, calls carry the identity of the machine it runs on (for a
// laptop: you). As an agent, it first joins the tailnet as its own ephemeral
// tagged node, so calls carry that tag's narrower grants instead of yours.
package mcpbridge

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"github.com/quinnovator/inhouse/internal/version"
)

func httpClient(tr http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     tr,
		Timeout:       130 * time.Second, // wait_for_operation may block 120s
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// RunAsDevice bridges stdio to base's MCP server over this device's own
// tailnet connection.
func RunAsDevice(ctx context.Context, base string) error {
	client := httpClient(http.DefaultTransport)
	defer client.CloseIdleConnections()
	return serve(ctx, base, client)
}

type Agent struct {
	Tag        string
	Credential tailnet.Credential
	StateDir   string // default: a temporary directory removed on exit
}

// DefaultAgentCredential reads ~/.config/inhouse/agent/{client-id,client-secret}.
func DefaultAgentCredential() (tailnet.Credential, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return tailnet.Credential{}, err
	}
	dir := filepath.Join(home, ".config", "inhouse", "agent")
	id, err := os.ReadFile(filepath.Join(dir, "client-id"))
	if err != nil {
		return tailnet.Credential{}, errors.New("agent OAuth client ID not found in " + dir)
	}
	return tailnet.Credential{ClientID: strings.TrimSpace(string(id)), SecretFile: filepath.Join(dir, "client-secret")}, nil
}

// RunAsAgent joins the tailnet as a fresh ephemeral node tagged a.Tag, then
// bridges stdio to base's MCP server from that node. The node logs out when
// the agent exits.
func RunAsAgent(ctx context.Context, base string, a Agent) error {
	if !strings.HasPrefix(a.Tag, "tag:") {
		return errors.New("agent tag must look like tag:name")
	}
	dir := a.StateDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "inhouse-agent-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}
	n, err := tailnet.Open(ctx, tailnet.NodeConfig{
		Dir: dir, Hostname: "inhouse-agent-" + uuid.NewString()[:8], Tag: a.Tag, Ephemeral: true, Enroll: a.Credential,
	})
	if err != nil {
		return err
	}
	defer func() {
		logout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = n.Client.Logout(logout)
		_ = n.Close()
	}()
	client := httpClient(&http.Transport{DialContext: n.Server.Dial, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second})
	defer client.CloseIdleConnections()
	return serve(ctx, base, client)
}

// serve mirrors the remote tool list once at startup and forwards each call.
func serve(ctx context.Context, base string, client *http.Client) error {
	remote := mcp.NewClient(&mcp.Implementation{Name: "inhouse-bridge", Version: version.Version}, nil)
	session, err := remote.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: strings.TrimRight(base, "/") + "/mcp", HTTPClient: client}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	local := mcp.NewServer(&mcp.Implementation{Name: "inhouse", Version: version.Version}, nil)
	for _, tool := range tools.Tools {
		name := tool.Name
		local.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: req.Params.Arguments})
		})
	}
	return local.Run(ctx, &mcp.StdioTransport{MaxLineLength: 256 << 10})
}
