package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// MemoryStore holds large tool outputs in Go RAM so they don't blow up the LLM context.
type MemoryStore struct {
	store map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{store: make(map[string]string)}
}

func (m *MemoryStore) Save(content string) string {
	bytes := make([]byte, 4)
	rand.Read(bytes)
	id := "mem_" + hex.EncodeToString(bytes)
	m.store[id] = content
	return id
}

// QueryMemoryToolInput defines the arguments for our native summarization tool.
type QueryMemoryToolInput struct {
	MemoryID    string `json:"memory_id" description:"The ID of the memory to query (e.g., mem_a1b2)"`
	Instruction string `json:"instruction" description:"Specific instructions on what to extract or summarize from this memory"`
}

// MCP <-> Fantasy Bridge

// MCPToolWrapper implements fantasy.AgentTool to bridge the AI SDK and MCP
type MCPToolWrapper struct {
	client     *client.Client
	mcpTool    mcp.Tool
	memory     *MemoryStore
	maxContext int
}

func (w *MCPToolWrapper) Info() fantasy.ToolInfo {
	var props map[string]any
	var req []string

	// Map MCP Schema exactly to what Fantasy ToolInfo expects.
	if len(w.mcpTool.InputSchema.Properties) > 0 {
		props = w.mcpTool.InputSchema.Properties
		req = w.mcpTool.InputSchema.Required
	} else if len(w.mcpTool.RawInputSchema) > 0 {
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
			fmt.Fprintf(&output, "%v\n", c)
		}
	}

	finalText := output.String()

	// MEMORY INTERCEPTOR LOGIC
	// Estimate tokens (roughly 4 chars per token).
	// If output takes up more than 40% of the total context size, we stash it.
	estimatedTokens := len(finalText) / 4
	threshold := int(float64(w.maxContext) * 0.40)

	if estimatedTokens > threshold && !res.IsError {
		memID := w.memory.Save(finalText)
		msg := fmt.Sprintf(
			"SUCCESS: The tool executed successfully, but the output is extremely large (~%d tokens). "+
				"To prevent context overflow, it has been saved to Go memory with ID: `%s`. "+
				"USE THE `query_memory` TOOL to search, summarize, or extract information from this memory ID.",
			estimatedTokens, memID,
		)
		return fantasy.NewTextResponse(msg), nil
	}

	if res.IsError {
		return fantasy.NewTextErrorResponse(finalText), nil
	}
	return fantasy.NewTextResponse(finalText), nil
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
		mgr.enabled[srv.Name] = false
	}

	for _, srv := range servers {
		var c *client.Client
		var err error

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
			continue
		}

		if err != nil {
			fmt.Printf("Failed to create client for %s: %v\n", srv.Name, err)
			continue
		}

		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "prmtr", Version: "1.0.0"}

		if _, err := c.Initialize(ctx, initReq); err != nil {
			fmt.Printf("Failed to initialize %s: %v\n", srv.Name, err)
		} else {
			mgr.clients[srv.Name] = c
			mgr.enabled[srv.Name] = true
		}
	}
	return mgr
}
