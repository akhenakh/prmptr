package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/crawlab-team/bm25"
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

type editorFinishedMsg struct {
	err  error
	file string
	name string
}

type feedbackMsg struct{}

// Stream Messaging
type streamTextMsg string
type streamToolCallMsg fantasy.ToolCallContent
type streamToolResultMsg fantasy.ToolResultContent
type streamDoneMsg struct{}
type streamErrMsg error

// interaction represents a single turn in the chat history
type interaction struct {
	Role        string
	System      string   // Only populated for User role
	MCPs        []string // Only populated for User role
	Model       string   // Only populated for User role
	Text        string
	ToolCalls   []fantasy.ToolCallContent
	ToolResults []fantasy.ToolResultContent
}

type appModel struct {
	config    *Config
	width     int
	height    int
	state     AppState
	isLoading bool

	// UI Components
	spinner      spinner.Model
	feedbackText string

	// Advanced Text Input
	input     []rune
	cursorIdx int
	newPrompt []rune

	// Session History & Navigation
	history       []interaction
	promptHistory []string
	historyIdx    int
	scrollOffset  int // 0 is bottom, >0 is scrolled up

	// Event Channels & Concurrency
	streamChan chan tea.Msg
	cancelGen  context.CancelFunc
	lastEsc    time.Time

	// Active Selections
	activeModel string
	activeSys   string
	mcpManager  *MCPManager
	memoryStore *MemoryStore // Holds large data chunks

	// Menus
	menuCursor int
	menuItems  []string

	// Cache
	glamourRenderer *glamour.TermRenderer
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
		streamChan:  make(chan tea.Msg, 100),
	}
}

// Bubble Tea v2 Lifecycle

func (m appModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func waitForStream(c chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-c
	}
}

func clearFeedback() tea.Msg {
	time.Sleep(3 * time.Second)
	return feedbackMsg{}
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

		// Initialize or update the renderer when window size changes
		width := m.width - 4
		if width < 10 {
			width = 80 // fallback
		}
		m.glamourRenderer, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(width),
		)

	// Streaming Handlers (Batched)
	case streamTextMsg, streamToolCallMsg, streamToolResultMsg, streamErrMsg, streamDoneMsg:
		processMsg := func(mMsg tea.Msg) {
			if len(m.history) == 0 {
				if errMsg, ok := mMsg.(streamErrMsg); ok {
					m.isLoading = false
					m.cancelGen = nil
					m.feedbackText = fmt.Sprintf("Error: %v", error(errMsg))
				} else if _, ok := mMsg.(streamDoneMsg); ok {
					m.isLoading = false
					m.cancelGen = nil
				}
				return
			}

			lastIdx := len(m.history) - 1
			switch nmsg := mMsg.(type) {
			case streamTextMsg:
				m.history[lastIdx].Text += string(nmsg)
				m.scrollOffset = 0
			case streamToolCallMsg:
				m.history[lastIdx].ToolCalls = append(m.history[lastIdx].ToolCalls, fantasy.ToolCallContent(nmsg))
				m.scrollOffset = 0
			case streamToolResultMsg:
				m.history[lastIdx].ToolResults = append(m.history[lastIdx].ToolResults, fantasy.ToolResultContent(nmsg))
				m.scrollOffset = 0
			case streamErrMsg:
				m.isLoading = false
				m.cancelGen = nil
				m.history[lastIdx].Text += fmt.Sprintf("\n\n**Error:** %v", error(nmsg))
			case streamDoneMsg:
				m.isLoading = false
				m.cancelGen = nil
			}
		}

		// Process the event that woke us up
		processMsg(msg)

		// Drain any queued events from the channel synchronously
		// to batch updates and drastically reduce UI render cycles
		for draining := true; draining; {
			select {
			case nextMsg := <-m.streamChan:
				processMsg(nextMsg)
			default:
				draining = false
			}
		}

		// Only wait for more chunks if the stream hasn't finished
		if m.isLoading {
			cmds = append(cmds, waitForStream(m.streamChan))
		}

	case feedbackMsg:
		m.feedbackText = ""

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

	// Mouse Scrolling Support
	case tea.MouseWheelMsg:
		if m.state == StateNormal {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.scrollOffset += 3 // Scroll up into history
			case tea.MouseWheelDown:
				m.scrollOffset -= 3 // Scroll down to latest
			}
		}

	// Keyboard Support
	case tea.KeyPressMsg:
		// Global Quit
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
					if len(m.history) > 0 {
						m.history[len(m.history)-1].Text += "\n\n*Cancelled by user.*"
					}
				}
				m.input = []rune{}
				m.cursorIdx = 0
				m.scrollOffset = 0
			}
			m.state = StateNormal
			m.lastEsc = now
			return m, tea.Batch(cmds...)
		}

		// Don't process normal input if we are waiting for LLM
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

		if msg.String() == "ctrl+n" {
			// New Session
			m.history = nil
			m.promptHistory = nil
			m.historyIdx = 0
			m.input = []rune{}
			m.cursorIdx = 0
			m.scrollOffset = 0
			m.memoryStore = NewMemoryStore()
			m.feedbackText = "New Session Started"
			cmds = append(cmds, clearFeedback)
			return m, tea.Batch(cmds...)
		}

		if msg.String() == "ctrl+s" {
			filename := saveHistoryToMarkdown(m.history)
			m.feedbackText = fmt.Sprintf("Saved to %s", filename)
			cmds = append(cmds, clearFeedback)
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

					// Add User message
					m.history = append(m.history, interaction{
						Role:   "User",
						Text:   strInput,
						System: sysPrompt,
						MCPs:   mcps,
						Model:  m.activeModel,
					})

					// Add empty Assistant placeholder
					m.history = append(m.history, interaction{
						Role: "Assistant",
						Text: "",
					})

					m.input = []rune{}
					m.cursorIdx = 0
					m.isLoading = true
					m.scrollOffset = 0

					// Run LLM call with a cancelable context
					ctx, cancel := context.WithCancel(context.Background())
					m.cancelGen = cancel

					// Start stream in background
					go m.startLLMStream(ctx, strInput, sysPrompt, mcps)
					cmds = append(cmds, waitForStream(m.streamChan))
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
			case "pgup", "pageup":
				m.scrollOffset += (m.height / 2)
			case "pgdown", "pagedown":
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
					f, _ := os.CreateTemp("", "prmptr-*.md")
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
	v.AltScreen = true

	// Using MouseModeNone enables Native Terminal text highlighting/copying.
	// We can still capture mouse wheels natively in most emulators with this.
	v.MouseMode = tea.MouseModeNone
	v.KeyboardEnhancements.ReportEventTypes = true

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
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true).MarginBottom(1)

	cursorBgStyle = lipgloss.NewStyle().Background(lipgloss.Color("212")).Foreground(lipgloss.Color("232"))
	cursorBlock   = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render("█")
)

func (m appModel) renderNormal() string {
	activeMCPs := m.getActiveMCPs()

	statusText := fmt.Sprintf(" Model: %s | Prompt: %s | MCP: %d | Mem: %d | ^P Menu | ^N New | ^S Save | esc esc Cancel",
		m.activeModel, m.activeSys, len(activeMCPs), len(m.memoryStore.store))

	var statusRender string
	if m.feedbackText != "" {
		statusRender = warnStyle.Render(" " + m.feedbackText)
	} else {
		if m.isLoading {
			statusText = fmt.Sprintf(" %s Generating... |%s", m.spinner.View(), statusText)
		} else {
			statusText = " " + statusText
		}
		statusRender = statusStyle.Render(statusText)
	}

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

	mdHeight := m.height - lipgloss.Height(statusRender) - lipgloss.Height(inputBox) - 2
	if mdHeight < 5 {
		mdHeight = 5
	}

	// Build Markdown String for the entire history
	var sb strings.Builder
	if len(m.history) == 0 {
		sb.WriteString("*Awaiting prompt... (You can use your mouse to select and copy text natively)*\n")
	}

	for _, item := range m.history {
		if item.Role == "User" {
			sb.WriteString("---\n")
			fmt.Fprintf(&sb, "**🤖 System:** *%s*\n", strings.ReplaceAll(item.System, "\n", " "))
			if len(item.MCPs) > 0 {
				fmt.Fprintf(&sb, "**🔌 MCPs:** `%s`\n", strings.Join(item.MCPs, "`, `"))
			}
			fmt.Fprintf(&sb, "**🧠 Model:** `%s`\n\n", item.Model)
			fmt.Fprintf(&sb, "**👤 User:**\n%s\n\n", item.Text)
		} else {
			sb.WriteString("**✨ Assistant:**\n")
			for _, tc := range item.ToolCalls {
				fmt.Fprintf(&sb, "> 🛠️ **Tool Call**: `%s`\n> Input: `%s`\n\n", tc.ToolName, tc.Input)
			}
			for _, tr := range item.ToolResults {
				if textRes, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					formatted := formatToolResult(textRes.Text, true)
					fmt.Fprintf(&sb, "> 📋 **Tool Result** (`%s`):\n", tr.ToolCallID)
					fmt.Fprintf(&sb, "> ```\n> %s\n> ```\n\n", strings.ReplaceAll(formatted, "\n", "\n> "))
				}
			}
			if item.Text != "" {
				sb.WriteString(item.Text + "\n\n")
			}
		}
	}

	var r *glamour.TermRenderer
	if m.glamourRenderer != nil {
		r = m.glamourRenderer
	} else {
		// Fallback in case View is called before the first WindowSizeMsg
		width := m.width - 4
		if width < 10 {
			width = 80
		}
		r, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(width),
		)
	}

	rendered, _ := r.Render(sb.String())

	lines := strings.Split(rendered, "\n")
	maxScroll := len(lines) - mdHeight
	if maxScroll < 0 {
		maxScroll = 0
	}

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

	content := lipgloss.JoinVertical(lipgloss.Left, topArea, statusRender, inputBox)
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
			m.memoryStore = NewMemoryStore()
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

func estimateTokens(text string) int {
	return len(text) / 4 // 1 token ~ 4 chars
}

// chunkText splits text into sizable string chunks
func chunkText(text string, chunkSize int) []string {
	lines := strings.Split(text, "\n")
	var chunks []string
	var currentChunk strings.Builder

	for _, line := range lines {
		if currentChunk.Len()+len(line) > chunkSize && currentChunk.Len() > 0 {
			chunks = append(chunks, currentChunk.String())
			currentChunk.Reset()
		}
		currentChunk.WriteString(line)
		currentChunk.WriteString("\n")
	}
	if currentChunk.Len() > 0 {
		chunks = append(chunks, currentChunk.String())
	}

	if len(chunks) == 1 && len(chunks[0]) > chunkSize*2 {
		var rawChunks []string
		runes := []rune(text)
		for i := 0; i < len(runes); i += chunkSize {
			end := i + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			rawChunks = append(rawChunks, string(runes[i:end]))
		}
		return rawChunks
	}
	return chunks
}

func formatToolResult(input string, truncate bool) string {
	formatted := input
	var data map[string]any

	if err := json.Unmarshal([]byte(input), &data); err == nil {
		found := false
		for _, key := range []string{"markdown", "content", "response", "data", "text"} {
			if val, ok := data[key]; ok {
				if strVal, isStr := val.(string); isStr {
					formatted = strVal
					found = true
					break
				}
			}
		}
		if !found {
			pretty, _ := json.MarshalIndent(data, "", "  ")
			formatted = "```json\n" + string(pretty) + "\n```"
		}
	}

	if truncate && len(formatted) > 2000 {
		return formatted[:2000] + "\n... (truncated for UI)"
	}
	return formatted
}

// saveHistoryToMarkdown saves the full untruncated conversation to a file.
func saveHistoryToMarkdown(history []interaction) string {
	if len(history) == 0 {
		return ""
	}

	var firstPrompt string
	for _, item := range history {
		if item.Role == "User" {
			firstPrompt = item.Text
			break
		}
	}

	safeName := strings.ReplaceAll(firstPrompt, " ", "_")
	safeName = strings.ReplaceAll(safeName, "/", "_")
	safeName = strings.ReplaceAll(safeName, "\n", "")
	if len(safeName) > 20 {
		safeName = safeName[:20]
	}

	filename := time.Now().Format("2006-01-02") + "_" + safeName + ".md"

	var sb strings.Builder
	for _, item := range history {
		if item.Role == "User" {
			fmt.Fprintf(&sb, "## User\n**System**: %s\n**Model**: %s\n\n%s\n\n", item.System, item.Model, item.Text)
		} else {
			sb.WriteString("## Assistant\n")
			for _, tc := range item.ToolCalls {
				fmt.Fprintf(&sb, "\n> **Tool Call**: `%s`\n> Input: `%s`\n", tc.ToolName, tc.Input)
			}
			for _, tr := range item.ToolResults {
				if textRes, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					fmt.Fprintf(&sb, "\n> **Tool Result** (`%s`):\n\n%s\n\n", tr.ToolCallID, formatToolResult(textRes.Text, false))
				}
			}
			sb.WriteString(item.Text + "\n\n")
		}
	}

	os.WriteFile(filename, []byte(sb.String()), 0644)
	return filename
}

func (m appModel) startLLMStream(ctx context.Context, prompt, sysContent string, activeMCPs []string) {
	var modelCfg *ModelConfig
	for _, mod := range m.config.Models {
		if mod.Name == m.activeModel {
			modelCfg = &mod
			break
		}
	}

	if modelCfg == nil {
		m.streamChan <- streamErrMsg(fmt.Errorf("model %s not found", m.activeModel))
		return
	}

	providerCfg, ok := m.config.Providers[modelCfg.Provider]
	if !ok {
		m.streamChan <- streamErrMsg(fmt.Errorf("provider %s not found", modelCfg.Provider))
		return
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
		m.streamChan <- streamErrMsg(fmt.Errorf("unsupported provider type: %s", modelCfg.Provider))
		return
	}

	if err != nil {
		m.streamChan <- streamErrMsg(err)
		return
	}

	langModel, err := provider.LanguageModel(ctx, modelCfg.Name)
	if err != nil {
		m.streamChan <- streamErrMsg(err)
		return
	}

	// Sliding Window Context Budget
	maxCtx := modelCfg.MaxContextSize
	if maxCtx <= 0 {
		maxCtx = 8192
	}
	budget := int(float64(maxCtx) * 0.75)

	var historyMsgs []fantasy.Message
	currentTokens := estimateTokens(sysContent) + estimateTokens(prompt)

	// Build Fantasy history context backwards
	for i := len(m.history) - 3; i >= 0; i-- {
		item := m.history[i]
		tokens := estimateTokens(item.Text)

		if currentTokens+tokens > budget {
			break
		}
		currentTokens += tokens

		role := fantasy.MessageRole("user")
		if item.Role == "Assistant" {
			role = fantasy.MessageRole("assistant")
		}

		var parts []fantasy.MessagePart
		if item.Text != "" {
			parts = append(parts, fantasy.TextPart{Text: item.Text})
		}
		for _, tc := range item.ToolCalls {
			parts = append(parts, fantasy.ToolCallPart{
				ToolCallID: tc.ToolCallID,
				ToolName:   tc.ToolName,
				Input:      tc.Input,
			})
		}
		for _, tr := range item.ToolResults {
			parts = append(parts, fantasy.ToolResultPart{
				ToolCallID: tr.ToolCallID,
				Output:     tr.Result,
			})
		}

		if len(parts) > 0 {
			historyMsgs = append(historyMsgs, fantasy.Message{Role: role, Content: parts})
		}
	}

	for i, j := 0, len(historyMsgs)-1; i < j; i, j = i+1, j-1 {
		historyMsgs[i], historyMsgs[j] = historyMsgs[j], historyMsgs[i]
	}

	// Setup Tools
	var agentTools []fantasy.AgentTool
	var activeToolNames []string

	// Create BM25-backed memory query tool
	queryMemTool := fantasy.NewAgentTool(
		"query_memory",
		"Query or summarize information from a large saved memory chunk. Use this when a tool returns a 'mem_...' ID instead of actual data.",
		func(ctx context.Context, input QueryMemoryToolInput, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			text, exists := m.memoryStore.store[input.MemoryID]
			if !exists {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Memory ID %s not found", input.MemoryID)), nil
			}

			usedBM25 := false
			summaryBudget := int(float64(maxCtx) * 0.7)

			// RAG / BM25 Logic
			if estimateTokens(text) > summaryBudget {
				chunks := chunkText(text, 2000)
				tokenizer := func(s string) []string {
					f := func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsNumber(c) }
					return strings.FieldsFunc(strings.ToLower(s), f)
				}

				qTokens := tokenizer(input.Instruction)
				if len(qTokens) > 0 {
					b, err := bm25.NewBM25Okapi(chunks, tokenizer, 1.5, 0.75, nil)
					if err == nil {
						nChunks := summaryBudget / 500
						if nChunks < 1 {
							nChunks = 1
						}
						if nChunks > len(chunks) {
							nChunks = len(chunks)
						}

						topChunks, err := b.GetTopN(qTokens, nChunks)
						if err == nil && len(topChunks) > 0 {
							text = strings.Join(topChunks, "\n\n...[SNIP]...\n\n")
							usedBM25 = true
						}
					}
				} else {
					maxChars := summaryBudget * 4
					if len(text) > maxChars {
						text = text[:maxChars] + "\n...[TRUNCATED]..."
					}
				}
			}

			queryAgent := fantasy.NewAgent(
				langModel,
				fantasy.WithSystemPrompt("You are a data extraction assistant. Extract information or summarize the following text according to the user's instructions. Keep your answer focused and concise."),
			)

			summaryPrompt := fmt.Sprintf("Instruction: %s\n\nData:\n%s", input.Instruction, text)
			res, err := queryAgent.Generate(ctx, fantasy.AgentCall{Prompt: summaryPrompt})
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to query memory: " + err.Error()), nil
			}

			finalRes := res.Response.Content.Text()
			if usedBM25 {
				finalRes = "⚠️ **WARNING:** The original memory was too large. BM25 retrieval was used to select relevant parts before summarization. Some details may have been missed.\n\n" + finalRes
			}

			return fantasy.NewTextResponse(finalRes), nil
		},
	)
	agentTools = append(agentTools, queryMemTool)
	activeToolNames = append(activeToolNames, "query_memory")

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
					memory:     m.memoryStore,
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

	// Execute streaming call
	call := fantasy.AgentStreamCall{
		Prompt:      prompt,
		Messages:    historyMsgs,
		ActiveTools: activeToolNames,
		OnTextDelta: func(id, text string) error {
			m.streamChan <- streamTextMsg(text)
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			m.streamChan <- streamToolCallMsg(tc)
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			m.streamChan <- streamToolResultMsg(tr)
			return nil
		},
	}

	_, err = agent.Stream(ctx, call)
	if err != nil && !errors.Is(err, context.Canceled) {
		m.streamChan <- streamErrMsg(err)
		return
	}

	m.streamChan <- streamDoneMsg{}
}
