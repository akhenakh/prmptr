package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

// -------------------------------------------------------------------------
// TUI State & Message Definitions
// -------------------------------------------------------------------------

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
	text string
	err  error
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

	// Current Input
	input     string
	newPrompt string

	// Session History & Navigation
	history       []interaction
	promptHistory []string
	historyIdx    int
	scrollOffset  int

	// Active Selections
	activeModel string
	activeSys   string
	mcpManager  *MCPManager

	// Menus
	menuCursor int
	menuItems  []string
}

func initialModel(cfg *Config) appModel {
	activeMod := "Unknown"
	if len(cfg.Models) > 0 {
		activeMod = cfg.Models[0].Name
	}
	activeSys := "default"
	if len(cfg.SystemPrompts) > 0 {
		activeSys = cfg.SystemPrompts[0].Name
	}

	return appModel{
		config:      cfg,
		state:       StateNormal,
		activeModel: activeMod,
		activeSys:   activeSys,
		mcpManager:  NewMCPManager(cfg.MCPServers),
		history:     make([]interaction, 0),
	}
}

// -------------------------------------------------------------------------
// Bubble Tea v2 Lifecycle
// -------------------------------------------------------------------------

func (m appModel) Init() tea.Cmd {
	return nil
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case llmResponseMsg:
		m.isLoading = false
		content := msg.text
		if msg.err != nil {
			content = fmt.Sprintf("**Error:** %v", msg.err)
		}

		m.history = append(m.history, interaction{
			Role:    "Assistant",
			Content: content,
		})
		m.scrollOffset = 999999 // Auto-scroll to bottom

	case editorFinishedMsg:
		if msg.err == nil {
			content, _ := os.ReadFile(msg.file)
			newPrompt := SystemPrompt{Name: msg.name, Content: string(content)}
			m.config.SystemPrompts = append(m.config.SystemPrompts, newPrompt)
			m.activeSys = msg.name
			_ = saveConfig("config.yaml", m.config)
		}
		os.Remove(msg.file)
		m.state = StateNormal

	// Mouse Scrolling Support
	case tea.MouseWheelMsg:
		if m.state == StateNormal {
			if msg.Button == tea.MouseWheelUp {
				m.scrollOffset -= 3
			} else if msg.Button == tea.MouseWheelDown {
				m.scrollOffset += 3
			}
		}

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		// Don't process input if we are waiting for LLM
		if m.isLoading {
			return m, nil
		}

		if msg.String() == "ctrl+p" {
			m.state = StateMainMenu
			m.menuCursor = 0
			m.menuItems = []string{
				"Switch Model",
				"Select System Prompt",
				"Add New Prompt",
				"Manage MCP Servers",
			}
			return m, nil
		}
		if msg.String() == "esc" {
			m.state = StateNormal
			return m, nil
		}

		switch m.state {
		case StateNormal:
			switch msg.String() {
			case "enter":
				if strings.TrimSpace(m.input) != "" {
					sysPrompt := m.getActiveSystemPrompt()
					mcps := m.getActiveMCPs()

					// Save to histories
					m.promptHistory = append(m.promptHistory, m.input)
					m.historyIdx = len(m.promptHistory)

					m.history = append(m.history, interaction{
						Role:    "User",
						Content: m.input,
						System:  sysPrompt,
						MCPs:    mcps,
						Model:   m.activeModel,
					})

					promptStr := m.input
					m.input = ""
					m.isLoading = true
					m.scrollOffset = 999999 // Auto-scroll to bottom

					cmd := m.requestLLM(promptStr, sysPrompt, mcps)
					return m, cmd
				}
			case "backspace":
				runes := []rune(m.input)
				if len(runes) > 0 {
					m.input = string(runes[:len(runes)-1])
				}
			case "space":
				m.input += " "
			case "up":
				if m.historyIdx > 0 {
					m.historyIdx--
					m.input = m.promptHistory[m.historyIdx]
				}
			case "down":
				if m.historyIdx < len(m.promptHistory)-1 {
					m.historyIdx++
					m.input = m.promptHistory[m.historyIdx]
				} else if m.historyIdx == len(m.promptHistory)-1 {
					m.historyIdx++
					m.input = ""
				}
			case "pgup":
				m.scrollOffset -= (m.height / 2)
			case "pgdown":
				m.scrollOffset += (m.height / 2)
			default:
				if len(msg.Text) > 0 {
					m.input += msg.Text
				}
			}

		case StatePromptNameInput:
			switch msg.String() {
			case "enter":
				if len(m.newPrompt) > 0 {
					f, _ := os.CreateTemp("", "prmtr-*.md")
					f.Close()

					editor := os.Getenv("EDITOR")
					if editor == "" {
						editor = "nano"
					}

					c := exec.Command(editor, f.Name())
					return m, tea.ExecProcess(c, func(err error) tea.Msg {
						return editorFinishedMsg{err: err, file: f.Name(), name: m.newPrompt}
					})
				}
			case "backspace":
				runes := []rune(m.newPrompt)
				if len(runes) > 0 {
					m.newPrompt = string(runes[:len(runes)-1])
				}
			case "space":
				m.newPrompt += " "
			default:
				if len(msg.Text) > 0 {
					m.newPrompt += msg.Text
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
				return m.handleMenuSelection()
			}
		}
	}

	return m, nil
}

// Declarative View rendering in Bubble Tea v2
func (m appModel) View() tea.View {
	var v tea.View
	v.AltScreen = true                    // Declarative alternate screen
	v.MouseMode = tea.MouseModeCellMotion // Enable mouse scroll wheel

	var ui string
	if m.state == StateNormal {
		ui = m.renderNormal()
	} else if m.state == StatePromptNameInput {
		ui = m.renderPromptNameInput()
	} else {
		ui = m.renderMenu()
	}

	v.SetContent(ui)
	return v
}

// -------------------------------------------------------------------------
// Layout & UI Renderers
// -------------------------------------------------------------------------

var (
	baseStyle   = lipgloss.NewStyle().Padding(1, 2)
	titleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	inputStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63")).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).MarginBottom(1)
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render("█")
)

func (m appModel) renderNormal() string {
	activeMCPs := m.getActiveMCPs()

	statusText := fmt.Sprintf(" Model: %s | Prompt: %s | MCP: %d active | (ctrl+p) menu", m.activeModel, m.activeSys, len(activeMCPs))
	if m.isLoading {
		statusText = " ⏳ Generating response... " + statusText
	}
	status := statusStyle.Render(statusText)

	inputBox := inputStyle.Width(m.width - 4).Render(m.input + cursorStyle)

	mdHeight := m.height - lipgloss.Height(status) - lipgloss.Height(inputBox) - 2
	if mdHeight < 5 {
		mdHeight = 5
	}

	// 1. Build Markdown String for the entire history
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
	if m.isLoading {
		sb.WriteString("**✨ Assistant:**\n*Generating response...*\n")
	}

	// 2. Render Markdown via Glamour
	r, _ := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(m.width-4),
	)
	rendered, _ := r.Render(sb.String())

	// 3. Handle Viewport Scrolling
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

	endIdx := m.scrollOffset + mdHeight
	if endIdx > len(lines) {
		endIdx = len(lines)
	}

	renderedPort := strings.Join(lines[m.scrollOffset:endIdx], "\n")

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
		titleStyle.Render("Enter New Prompt Name:") + "\n\n" + m.newPrompt + cursorStyle,
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

// -------------------------------------------------------------------------
// Action Handlers & Logic
// -------------------------------------------------------------------------

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
			m.newPrompt = ""
		case 3:
			m.state = StateMCPManage
			m.menuItems = make([]string, len(m.config.MCPServers))
			for i, srv := range m.config.MCPServers {
				m.menuItems[i] = srv.Name
			}
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

// requestLLM uses charm.land/fantasy to route requests and tools!
func (m appModel) requestLLM(prompt, sysContent string, activeMCPs []string) tea.Cmd {
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

		// Map to fantasy providers
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

		langModel, err := provider.LanguageModel(context.Background(), modelCfg.Name)
		if err != nil {
			return llmResponseMsg{err: err}
		}

		// Prepare active MCP tools mapped to Fantasy AgentTools
		var agentTools []fantasy.AgentTool
		var activeToolNames []string

		for _, srvName := range activeMCPs {
			client := m.mcpManager.clients[srvName]
			if client == nil {
				continue
			}

			// Get tool list from MCP
			res, err := client.ListTools(context.Background(), mcp.ListToolsRequest{})
			if err == nil {
				for _, t := range res.Tools {
					agentTools = append(agentTools, &MCPToolWrapper{
						client:  client,
						mcpTool: t,
					})
					activeToolNames = append(activeToolNames, t.Name)
				}
			}
		}

		// Create the Fantasy Agent with tools and the system prompt
		agent := fantasy.NewAgent(
			langModel,
			fantasy.WithTools(agentTools...),
			fantasy.WithSystemPrompt(sysContent),
		)

		call := fantasy.AgentCall{
			Prompt:      prompt,
			ActiveTools: activeToolNames,
		}

		// Execute Fantasy Agent (handles routing to provider AND intercepting MCP tool calls)
		result, err := agent.Generate(context.Background(), call)
		if err != nil {
			return llmResponseMsg{err: err}
		}

		return llmResponseMsg{text: result.Response.Content.Text()}
	}
}
