package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

// TUI State & Message Definitions

type AppState int

const (
	StateNormal AppState = iota
	StateMainMenu
	StateModelSelect
	StateSysSelect
	StateMCPManage
	StatePromptNameInput
)

type llmResponseMsg struct {
	result *fantasy.AgentResult
	err    error
}

type editorFinishedMsg struct {
	err  error
	file string
	name string
}

// interaction represents a single turn in the chat history
type interaction struct {
	Role    string
	Content string
	System  string   // Only populated for User role
	MCPs    []string // Only populated for User role
	Model   string   // Only populated for User role
}

type appModel struct {
	config    *Config
	width     int
	height    int
	state     AppState
	isLoading bool

	// UI Components
	spinner spinner.Model

	// Advanced Text Input
	input     []rune
	cursorIdx int
	newPrompt []rune

	// Session History & Navigation
	history       []interaction
	promptHistory []string
	historyIdx    int
	scrollOffset  int // 0 is bottom, >0 is scrolled up

	// Cancellations & Hotkeys
	cancelGen context.CancelFunc
	lastEsc   time.Time

	// Active Selections
	activeModel string
	activeSys   string
	mcpManager  *MCPManager
	memoryStore *MemoryStore // Holds large data chunks

	// Menus
	menuCursor int
	menuItems  []string
}

func initialModel(cfg *Config) appModel {
	activeMod := "Unknown"
	if len(cfg.Models) > 0 {
		activeMod = cfg.Models[0].Name
	}

	// Default prompt should be "default" if it exists, else the first one
	activeSys := ""
	for _, p := range cfg.SystemPrompts {
		if p.Name == "default" {
			activeSys = p.Name
			break
		}
	}
	if activeSys == "" && len(cfg.SystemPrompts) > 0 {
		activeSys = cfg.SystemPrompts[0].Name
	}

	// Initialize Spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return appModel{
		config:      cfg,
		state:       StateNormal,
		activeModel: activeMod,
		activeSys:   activeSys,
		mcpManager:  NewMCPManager(cfg.MCPServers),
		memoryStore: NewMemoryStore(),
		history:     make([]interaction, 0),
		input:       make([]rune, 0),
		spinner:     s,
	}
}

// Bubble Tea v2 Lifecycle

func (m appModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Unconditionally update the spinner so it animates smoothly
	var spinCmd tea.Cmd
	m.spinner, spinCmd = m.spinner.Update(msg)
	if spinCmd != nil {
		cmds = append(cmds, spinCmd)
	}

	// Main Event Dispatcher
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case llmResponseMsg:
		m.isLoading = false
		m.cancelGen = nil
		m.scrollOffset = 0 // Auto-scroll to bottom

		if msg.err != nil {
			m.history = append(m.history, interaction{
				Role:    "Assistant",
				Content: fmt.Sprintf("**Error:** %v", msg.err),
			})
		} else if msg.result != nil {
			m.history = append(m.history, interaction{
				Role:    "Assistant",
				Content: buildAssistantResponse(msg.result),
			})
		}

	case editorFinishedMsg:
		if msg.err == nil {
			content, _ := os.ReadFile(msg.file)
			newPrmt := SystemPrompt{Name: msg.name, Content: string(content)}
			m.config.SystemPrompts = append(m.config.SystemPrompts, newPrmt)
			m.activeSys = msg.name
			_ = saveConfig("prmptr.yaml", m.config)
		}
		os.Remove(msg.file)
		m.state = StateNormal

	// Pasting
	case tea.PasteMsg:
		if m.state == StateNormal {
			runes := []rune(msg.Content)
			m.input = append(m.input[:m.cursorIdx], append(runes, m.input[m.cursorIdx:]...)...)
			m.cursorIdx += len(runes)
		}

	// Mouse Scrolling
	case tea.MouseWheelMsg:
		if m.state == StateNormal {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.scrollOffset += 3
			case tea.MouseWheelDown:
				m.scrollOffset -= 3
			}
		}

	// Keyboard Support
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		if msg.String() == "esc" {
			now := time.Now()
			// Double ESC to cancel generation or clear input
			if now.Sub(m.lastEsc) < 500*time.Millisecond {
				if m.cancelGen != nil {
					m.cancelGen()
					m.cancelGen = nil
					m.isLoading = false
					m.history = append(m.history, interaction{
						Role:    "Assistant",
						Content: "*Cancelled by user.*",
					})
				}
				m.input = []rune{}
				m.cursorIdx = 0
				m.scrollOffset = 0
			}
			m.state = StateNormal
			m.lastEsc = now
			return m, tea.Batch(cmds...)
		}

		// Don't process other inputs if we are waiting for the LLM
		if m.isLoading {
			return m, tea.Batch(cmds...)
		}

		if msg.String() == "ctrl+p" {
			m.state = StateMainMenu
			m.menuCursor = 0
			m.menuItems = []string{
				"Switch Model",
				"Select System Prompt",
				"Add New Prompt",
				"Manage MCP Servers",
				"New Session",
			}
			return m, tea.Batch(cmds...)
		}

		switch m.state {
		case StateNormal:
			switch msg.String() {
			case "enter":
				strInput := strings.TrimSpace(string(m.input))
				if strInput != "" {
					sysPrompt := m.getActiveSystemPrompt()
					mcps := m.getActiveMCPs()

					m.promptHistory = append(m.promptHistory, strInput)
					m.historyIdx = len(m.promptHistory)

					m.history = append(m.history, interaction{
						Role:    "User",
						Content: strInput,
						System:  sysPrompt,
						MCPs:    mcps,
						Model:   m.activeModel,
					})

					m.input = []rune{}
					m.cursorIdx = 0
					m.isLoading = true
					m.scrollOffset = 0 // Snap to bottom

					// Run LLM call with a cancelable context
					ctx, cancel := context.WithCancel(context.Background())
					m.cancelGen = cancel

					llmCmd := m.requestLLM(ctx, strInput, sysPrompt, mcps)
					cmds = append(cmds, llmCmd)
					return m, tea.Batch(cmds...)
				}
			case "shift+enter", "alt+enter":
				m.input = append(m.input[:m.cursorIdx], append([]rune{'\n'}, m.input[m.cursorIdx:]...)...)
				m.cursorIdx++
			case "backspace":
				if m.cursorIdx > 0 {
					m.input = append(m.input[:m.cursorIdx-1], m.input[m.cursorIdx:]...)
					m.cursorIdx--
				}
			case "delete":
				if m.cursorIdx < len(m.input) {
					m.input = append(m.input[:m.cursorIdx], m.input[m.cursorIdx+1:]...)
				}
			case "left":
				if m.cursorIdx > 0 {
					m.cursorIdx--
				}
			case "right":
				if m.cursorIdx < len(m.input) {
					m.cursorIdx++
				}
			case "up":
				if m.historyIdx > 0 {
					m.historyIdx--
					m.input = []rune(m.promptHistory[m.historyIdx])
					m.cursorIdx = len(m.input)
				}
			case "down":
				if m.historyIdx < len(m.promptHistory)-1 {
					m.historyIdx++
					m.input = []rune(m.promptHistory[m.historyIdx])
					m.cursorIdx = len(m.input)
				} else if m.historyIdx == len(m.promptHistory)-1 {
					m.historyIdx++
					m.input = []rune{}
					m.cursorIdx = 0
				}
			case "ctrl+a", "home":
				m.cursorIdx = 0
			case "ctrl+e", "end":
				m.cursorIdx = len(m.input)
			case "pgup":
				m.scrollOffset += (m.height / 2)
			case "pgdown":
				m.scrollOffset -= (m.height / 2)
			case "space":
				m.input = append(m.input[:m.cursorIdx], append([]rune{' '}, m.input[m.cursorIdx:]...)...)
				m.cursorIdx++
			default:
				if len(msg.Text) > 0 {
					runes := []rune(msg.Text)
					m.input = append(m.input[:m.cursorIdx], append(runes, m.input[m.cursorIdx:]...)...)
					m.cursorIdx += len(runes)
				}
			}

		case StatePromptNameInput:
			switch msg.String() {
			case "enter":
				nameStr := string(m.newPrompt)
				if len(strings.TrimSpace(nameStr)) > 0 {
					f, _ := os.CreateTemp("", "prmtr-*.md")
					f.Close()

					editor := os.Getenv("EDITOR")
					if editor == "" {
						editor = "nano"
					}

					c := exec.Command(editor, f.Name())
					execCmd := tea.ExecProcess(c, func(err error) tea.Msg {
						return editorFinishedMsg{err: err, file: f.Name(), name: nameStr}
					})
					cmds = append(cmds, execCmd)
					return m, tea.Batch(cmds...)
				}
			case "backspace":
				if len(m.newPrompt) > 0 {
					m.newPrompt = m.newPrompt[:len(m.newPrompt)-1]
				}
			case "space":
				m.newPrompt = append(m.newPrompt, ' ')
			default:
				if len(msg.Text) > 0 {
					m.newPrompt = append(m.newPrompt, []rune(msg.Text)...)
				}
			}

		case StateMainMenu, StateModelSelect, StateSysSelect, StateMCPManage:
			switch msg.String() {
			case "up":
				if m.menuCursor > 0 {
					m.menuCursor--
				}
			case "down":
				if m.menuCursor < len(m.menuItems)-1 {
					m.menuCursor++
				}
			case "enter":
				mod, cmd := m.handleMenuSelection()
				cmds = append(cmds, cmd)
				return mod, tea.Batch(cmds...)
			}
		}
	}

	return m, tea.Batch(cmds...)
}

// Declarative View rendering in Bubble Tea v2
func (m appModel) View() tea.View {
	var v tea.View
	v.AltScreen = true                             // Declarative alternate screen
	v.MouseMode = tea.MouseModeCellMotion          // Enable mouse scroll wheel
	v.KeyboardEnhancements.ReportEventTypes = true // Support shift+enter, etc.

	var ui string
	switch m.state {
	case StateNormal:
		ui = m.renderNormal()
	case StatePromptNameInput:
		ui = m.renderPromptNameInput()
	default:
		ui = m.renderMenu()
	}

	v.SetContent(ui)
	return v
}

// Layout & UI Renderers

var (
	baseStyle   = lipgloss.NewStyle().Padding(1, 2)
	titleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	inputStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63")).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).MarginBottom(1)

	cursorBgStyle = lipgloss.NewStyle().Background(lipgloss.Color("212")).Foreground(lipgloss.Color("232"))
	cursorBlock   = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render("█")
)

func (m appModel) renderNormal() string {
	activeMCPs := m.getActiveMCPs()

	statusText := fmt.Sprintf(" Model: %s | Prompt: %s | MCP: %d active | Mem: %d items | (esc esc) cancel",
		m.activeModel, m.activeSys, len(activeMCPs), len(m.memoryStore.store))

	// Prepend the spinner to the status bar if loading
	if m.isLoading {
		statusText = fmt.Sprintf(" %s Generating response... |%s", m.spinner.View(), statusText)
	} else {
		statusText = " " + statusText
	}
	status := statusStyle.Render(statusText)

	// Render multiline input with cursor
	var inBuilder strings.Builder
	for i, r := range m.input {
		if i == m.cursorIdx {
			inBuilder.WriteString(cursorBgStyle.Render(string(r)))
		} else {
			inBuilder.WriteRune(r)
		}
	}
	if m.cursorIdx == len(m.input) {
		inBuilder.WriteString(cursorBlock)
	}
	inputBox := inputStyle.Width(m.width - 4).Render(inBuilder.String())

	mdHeight := m.height - lipgloss.Height(status) - lipgloss.Height(inputBox) - 2
	if mdHeight < 5 {
		mdHeight = 5
	}

	// Build Markdown String for the entire history
	var sb strings.Builder
	if len(m.history) == 0 {
		sb.WriteString("*Awaiting prompt...*\n")
	}

	for _, item := range m.history {
		if item.Role == "User" {
			sb.WriteString("---\n")
			sb.WriteString(fmt.Sprintf("**🤖 System:** *%s*\n", strings.ReplaceAll(item.System, "\n", " ")))
			if len(item.MCPs) > 0 {
				sb.WriteString(fmt.Sprintf("**🔌 MCPs:** `%s`\n", strings.Join(item.MCPs, "`, `")))
			}
			sb.WriteString(fmt.Sprintf("**🧠 Model:** `%s`\n\n", item.Model))
			sb.WriteString(fmt.Sprintf("**👤 User:**\n%s\n\n", item.Content))
		} else {
			sb.WriteString(fmt.Sprintf("**✨ Assistant:**\n%s\n\n", item.Content))
		}
	}

	// Render Markdown via Glamour
	r, _ := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(m.width-4),
	)
	rendered, _ := r.Render(sb.String())

	// Handle Viewport Scrolling (0 means bottom)
	lines := strings.Split(rendered, "\n")
	maxScroll := len(lines) - mdHeight
	if maxScroll < 0 {
		maxScroll = 0
	}

	// Clamp scroll offset
	if m.scrollOffset > maxScroll {
		m.scrollOffset = maxScroll
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}

	startIdx := maxScroll - m.scrollOffset
	if startIdx < 0 {
		startIdx = 0
	}
	endIdx := startIdx + mdHeight
	if endIdx > len(lines) {
		endIdx = len(lines)
	}

	renderedPort := strings.Join(lines[startIdx:endIdx], "\n")

	topArea := lipgloss.Place(
		m.width-4, mdHeight,
		lipgloss.Left, lipgloss.Bottom,
		renderedPort,
	)

	content := lipgloss.JoinVertical(lipgloss.Left, topArea, status, inputBox)
	return baseStyle.Render(content)
}

func (m appModel) renderPromptNameInput() string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2).Render(
		titleStyle.Render("Enter New Prompt Name:") + "\n\n" + string(m.newPrompt) + cursorBlock,
	)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m appModel) renderMenu() string {
	var s strings.Builder
	title := "Main Menu"

	switch m.state {
	case StateModelSelect:
		title = "Select Model"
	case StateSysSelect:
		title = "Select System Prompt"
	case StateMCPManage:
		title = "Manage MCP Servers"
	}

	s.WriteString(titleStyle.Render(title) + "\n\n")

	for i, item := range m.menuItems {
		cursor := "  "
		if i == m.menuCursor {
			cursor = "> "
		}

		if m.state == StateMCPManage {
			status := "[ ]"
			if m.mcpManager.enabled[item] {
				status = "[x]"
			}
			s.WriteString(fmt.Sprintf("%s%s %s\n", cursor, status, item))
		} else {
			s.WriteString(fmt.Sprintf("%s%s\n", cursor, item))
		}
	}
	s.WriteString("\n(esc to return)")

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 4).Render(s.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// Action Handlers & Logic

func (m *appModel) handleMenuSelection() (tea.Model, tea.Cmd) {
	switch m.state {
	case StateMainMenu:
		switch m.menuCursor {
		case 0:
			m.state = StateModelSelect
			m.menuItems = make([]string, len(m.config.Models))
			for i, mod := range m.config.Models {
				m.menuItems[i] = mod.Name
			}
		case 1:
			m.state = StateSysSelect
			m.menuItems = make([]string, len(m.config.SystemPrompts))
			for i, p := range m.config.SystemPrompts {
				m.menuItems[i] = p.Name
			}
		case 2:
			m.state = StatePromptNameInput
			m.newPrompt = []rune{}
		case 3:
			m.state = StateMCPManage
			m.menuItems = make([]string, len(m.config.MCPServers))
			for i, srv := range m.config.MCPServers {
				m.menuItems[i] = srv.Name
			}
		case 4: // New Session
			m.state = StateNormal
			m.history = nil
			m.promptHistory = nil
			m.historyIdx = 0
			m.input = []rune{}
			m.cursorIdx = 0
			m.scrollOffset = 0
			m.memoryStore = NewMemoryStore() // reset memory
		}
		m.menuCursor = 0

	case StateModelSelect:
		m.activeModel = m.menuItems[m.menuCursor]
		m.state = StateNormal

	case StateSysSelect:
		m.activeSys = m.menuItems[m.menuCursor]
		m.state = StateNormal

	case StateMCPManage:
		srvName := m.menuItems[m.menuCursor]
		m.mcpManager.enabled[srvName] = !m.mcpManager.enabled[srvName]
	}

	return *m, nil
}

func (m appModel) getActiveSystemPrompt() string {
	for _, p := range m.config.SystemPrompts {
		if p.Name == m.activeSys {
			return p.Content
		}
	}
	return "You are a helpful AI assistant."
}

func (m appModel) getActiveMCPs() []string {
	var active []string
	for name, enabled := range m.mcpManager.enabled {
		if enabled {
			active = append(active, name)
		}
	}
	return active
}

// buildAssistantResponse safely extracts all steps and Tool Calls/Results from Fantasy
func buildAssistantResponse(res *fantasy.AgentResult) string {
	var sb strings.Builder
	for _, step := range res.Steps {
		for _, msg := range step.Messages {
			switch msg.Role {
			case fantasy.MessageRoleAssistant:
				for _, part := range msg.Content {
					if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
						sb.WriteString(fmt.Sprintf("\n> 🛠️ **Tool Call**: `%s`\n> Input: `%s`\n\n", tc.ToolName, tc.Input))
					}
				}
			case fantasy.MessageRoleTool:
				for _, part := range msg.Content {
					if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
						sb.WriteString(fmt.Sprintf("\n> 📋 **Tool Result** (`%s`):\n", tr.ToolCallID))
						if textRes, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
							out := textRes.Text
							if len(out) > 2000 {
								out = out[:2000] + "\n... (truncated)"
							}
							sb.WriteString(fmt.Sprintf("> ```\n> %s\n> ```\n\n", strings.ReplaceAll(out, "\n", "\n> ")))
						}
					}
				}
			}
		}
	}
	finalText := res.Response.Content.Text()
	if finalText != "" {
		sb.WriteString("\n" + finalText)
	}
	return strings.TrimSpace(sb.String())
}

func estimateTokens(text string) int {
	return len(text) / 4 // 1 token ~ 4 chars
}

// requestLLM uses charm.land/fantasy to route requests and tools!
func (m appModel) requestLLM(ctx context.Context, prompt, sysContent string, activeMCPs []string) tea.Cmd {
	return func() tea.Msg {
		var modelCfg *ModelConfig
		for _, mod := range m.config.Models {
			if mod.Name == m.activeModel {
				modelCfg = &mod
				break
			}
		}

		if modelCfg == nil {
			return llmResponseMsg{err: fmt.Errorf("model %s not found", m.activeModel)}
		}

		providerCfg, ok := m.config.Providers[modelCfg.Provider]
		if !ok {
			return llmResponseMsg{err: fmt.Errorf("provider %s not found", modelCfg.Provider)}
		}

		var provider fantasy.Provider
		var err error

		switch modelCfg.Provider {
		case "openai":
			provider, err = openai.New(openai.WithAPIKey(providerCfg.APIKey))
		case "ollama":
			url := providerCfg.BaseURL
			if !strings.HasSuffix(url, "/v1") {
				url = strings.TrimRight(url, "/") + "/v1"
			}
			provider, err = openaicompat.New(
				openaicompat.WithBaseURL(url),
				openaicompat.WithAPIKey("ollama-compat"),
			)
		default:
			return llmResponseMsg{err: fmt.Errorf("unsupported provider type: %s", modelCfg.Provider)}
		}

		if err != nil {
			return llmResponseMsg{err: err}
		}

		langModel, err := provider.LanguageModel(ctx, modelCfg.Name)
		if err != nil {
			return llmResponseMsg{err: err}
		}

		// Sliding window for Conversation History
		maxCtx := modelCfg.MaxContextSize
		if maxCtx <= 0 {
			maxCtx = 8192
		}

		// Reserve 25% of context for system prompt, generated output, and tool schemas
		budget := int(float64(maxCtx) * 0.75)

		var historyMsgs []fantasy.Message
		currentTokens := estimateTokens(sysContent) + estimateTokens(prompt)

		// Loop backwards over history. `m.history` includes the user's latest prompt as the last element.
		// So we skip the very last element (len-1) because we'll pass it in the explicit `Prompt` field.
		for i := len(m.history) - 2; i >= 0; i-- {
			item := m.history[i]
			tokens := estimateTokens(item.Content)

			if currentTokens+tokens > budget {
				break // Stop adding older messages, budget reached
			}
			currentTokens += tokens

			role := fantasy.MessageRole("user")
			if item.Role == "Assistant" {
				role = fantasy.MessageRole("assistant")
			}

			historyMsgs = append(historyMsgs, fantasy.Message{
				Role: role,
				Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: item.Content},
				},
			})
		}

		// Reverse it back since we appended backwards
		for i, j := 0, len(historyMsgs)-1; i < j; i, j = i+1, j-1 {
			historyMsgs[i], historyMsgs[j] = historyMsgs[j], historyMsgs[i]
		}

		// Setup Tools
		var agentTools []fantasy.AgentTool
		var activeToolNames []string

		// Add the Native Query Memory Tool
		queryMemTool := fantasy.NewAgentTool(
			"query_memory",
			"Query or summarize information from a large saved memory chunk. Use this when a tool returns a 'mem_...' ID instead of actual data.",
			func(ctx context.Context, input QueryMemoryToolInput, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
				text, exists := m.memoryStore.store[input.MemoryID]
				if !exists {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Memory ID %s not found", input.MemoryID)), nil
				}

				// Spawn stateless summarizer
				queryAgent := fantasy.NewAgent(
					langModel,
					fantasy.WithSystemPrompt("You are a data extraction assistant. Extract information or summarize the following text according to the user's instructions. Keep your answer focused and concise."),
				)

				summaryPrompt := fmt.Sprintf("Instruction: %s\n\nData:\n%s", input.Instruction, text)
				res, err := queryAgent.Generate(ctx, fantasy.AgentCall{Prompt: summaryPrompt})
				if err != nil {
					return fantasy.NewTextErrorResponse("Failed to query memory: " + err.Error()), nil
				}

				return fantasy.NewTextResponse(res.Response.Content.Text()), nil
			},
		)
		agentTools = append(agentTools, queryMemTool)
		activeToolNames = append(activeToolNames, "query_memory")

		// Add Active MCP Tools
		for _, srvName := range activeMCPs {
			client := m.mcpManager.clients[srvName]
			if client == nil {
				continue
			}

			res, err := client.ListTools(ctx, mcp.ListToolsRequest{})
			if err == nil {
				for _, t := range res.Tools {
					agentTools = append(agentTools, &MCPToolWrapper{
						client:     client,
						mcpTool:    t,
						memory:     m.memoryStore, // Pass the memory store to the wrapper
						maxContext: maxCtx,
					})
					activeToolNames = append(activeToolNames, t.Name)
				}
			}
		}

		agent := fantasy.NewAgent(
			langModel,
			fantasy.WithTools(agentTools...),
			fantasy.WithSystemPrompt(sysContent),
		)

		call := fantasy.AgentCall{
			Prompt:      prompt,      // Explicitly set the prompt here!
			Messages:    historyMsgs, // Appended history sliding window
			ActiveTools: activeToolNames,
		}

		result, err := agent.Generate(ctx, call)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil // Discard message since it's already handled via ESC ESC
			}
			return llmResponseMsg{err: err}
		}

		return llmResponseMsg{result: result}
	}
}
