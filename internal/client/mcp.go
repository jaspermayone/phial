package client

import (
	"context"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServeMCP runs the local MCP server over stdio. It offers brief and
// verify, the two tools that need a checkout; everything else is on the
// server's own MCP endpoint.
func ServeMCP(ctx context.Context, api *API, version string) error {
	s := mcp.NewServer(&mcp.Implementation{Name: "phial-client", Version: version}, &mcp.ServerOptions{
		Instructions: "phial-client adds two local tools to phial's memory: brief gathers the phials and the code map for a task in this checkout, and verify checks the phials' code anchors against this checkout and reports stale ones to the server. For reading and writing memory use the phial server's own tools.",
	})
	type briefArgs struct {
		Task string `json:"task" jsonschema:"what you are about to do, in a few words"`
		Dir  string `json:"dir,omitempty" jsonschema:"the checkout; default is the current directory"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "brief", Description: "Context for a task: the phials about this project and everywhere, plus entire-graph's code map when it is installed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a briefArgs) (*mcp.CallToolResult, any, error) {
			dir := a.Dir
			if dir == "" {
				dir, _ = os.Getwd()
			}
			out, err := Brief(ctx, api, dir, a.Task)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
		})
	type verifyArgs struct {
		Dir string `json:"dir,omitempty" jsonschema:"the checkout; default is the current directory"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "verify", Description: "Check every phial anchored in this repo against the checkout and report the result to the server."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a verifyArgs) (*mcp.CallToolResult, any, error) {
			dir := a.Dir
			if dir == "" {
				dir, _ = os.Getwd()
			}
			checked, changed, err := VerifyRepo(ctx, api, dir, func(string, ...any) {})
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
			}
			msg := "Checked " + itoa(checked) + " phial(s); " + itoa(changed) + " changed state."
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
		})
	return s.Run(ctx, &mcp.StdioTransport{})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
