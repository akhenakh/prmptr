package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"
)

// Config & Data Structures
type Config struct {
	Providers     map[string]ProviderConfig `yaml:"providers"`
	Models        []ModelConfig             `yaml:"models"`
	MCPServers    []MCPServerConfig         `yaml:"mcp_servers"`
	SystemPrompts []SystemPrompt            `yaml:"system_prompts"`
}

type ProviderConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type ModelConfig struct {
	Name           string `yaml:"name"`
	Provider       string `yaml:"provider"`
	MaxContextSize int    `yaml:"max_context_size"`
}

type MCPServerConfig struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"` // "stdio" or "sse"
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	URL     string   `yaml:"url,omitempty"`
}

type SystemPrompt struct {
	Name    string `yaml:"name"`
	Content string `yaml:"content"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func main() {
	cfg, err := loadConfig("prmptr.yaml")
	if err != nil {
		fmt.Printf("Failed to load config (prmptr.yaml): %v\n", err)
		os.Exit(1)
	}

	p := tea.NewProgram(initialModel(cfg))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running prmtr: %v\n", err)
		os.Exit(1)
	}
}
