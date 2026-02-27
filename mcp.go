package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCP <-> Fantasy Bridge

// MCPToolWrapper implements fantasy.AgentTool to bridge the AI SDK and MCP
type MCPToolWrapper struct {
	client  *client.Client
	mcpTool mcp.Tool
}

func (w *MCPToolWrapper) Info() fantasy.ToolInfo {
	var props map[string]any
	var req []string

	// Map MCP Schema exactly to what Fantasy ToolInfo expects.
	// Fantasy's Parameters field expects the inner `properties` map of the schema.
	if len(w.mcpTool.InputSchema.Properties) > 0 {
		props = w.mcpTool.InputSchema.Properties
		req = w.mcpTool.InputSchema.Required
	} else if len(w.mcpTool.RawInputSchema) > 0 {
		// Fallback if the tool only provided a raw schema
		var raw map[string]any
		if err := json.Unmarshal(w.mcpTool.RawInputSchema, &raw); err == nil {
			if p, ok := raw["properties"].(map[string]any); ok {
				props = p
			}
			if r, ok := raw["required"].([]any); ok {
				for _, v := range r {
					if s, ok := v.(string); ok {
						req = append(req, s)
					}
				}
			}
		}
	}

	if props == nil {
		props = make(map[string]any)
	}

	return fantasy.ToolInfo{
		Name:        w.mcpTool.Name,
		Description: w.mcpTool.Description,
		Parameters:  props,
		Required:    req,
	}
}

func (w *MCPToolWrapper) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args map[string]any

	// If the LLM didn't pass any arguments, it might pass an empty string
	input := strings.TrimSpace(call.Input)
	if input == "" {
		input = "{}"
	}

	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid json input: %v", err)), nil
	}

	req := mcp.CallToolRequest{
		Request: mcp.Request{Method: string(mcp.MethodToolsCall)},
		Params: mcp.CallToolParams{
			Name:      w.mcpTool.Name,
			Arguments: args,
		},
	}

	res, err := w.client.CallTool(ctx, req)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	var output strings.Builder
	for _, content := range res.Content {
		switch c := content.(type) {
		case mcp.TextContent:
			output.WriteString(c.Text)
			output.WriteString("\n")
		default:
			output.WriteString(fmt.Sprintf("%v\n", c))
		}
	}

	if res.IsError {
		return fantasy.NewTextErrorResponse(output.String()), nil
	}
	return fantasy.NewTextResponse(output.String()), nil
}

func (w *MCPToolWrapper) ProviderOptions() fantasy.ProviderOptions        { return nil }
func (w *MCPToolWrapper) SetProviderOptions(opts fantasy.ProviderOptions) {}

// MCPManager handles standard MCP Go Clients
type MCPManager struct {
	clients map[string]*client.Client
	enabled map[string]bool
}

func NewMCPManager(servers []MCPServerConfig) *MCPManager {
	mgr := &MCPManager{
		clients: make(map[string]*client.Client),
		enabled: make(map[string]bool),
	}
	ctx := context.Background()

	for _, srv := range servers {
		var c *client.Client
		var err error

		// Default to stdio if type is not specified and command is present
		srvType := srv.Type
		if srvType == "" && srv.Command != "" {
			srvType = "stdio"
		}

		switch srvType {
		case "stdio":
			c, err = client.NewStdioMCPClient(srv.Command, nil, srv.Args...)
		case "sse":
			c, err = client.NewSSEMCPClient(srv.URL)
			if err == nil {
				err = c.Start(ctx)
			}
		default:
			continue // Skip servers with unsupported type
		}

		if err != nil {
			continue // Skip failing servers
		}

		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "prmtr", Version: "1.0.0"}

		if _, err := c.Initialize(ctx, initReq); err == nil {
			mgr.clients[srv.Name] = c
			mgr.enabled[srv.Name] = true
		}
	}
	return mgr
}
