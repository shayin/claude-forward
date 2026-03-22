package client

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 客户端配置
type Config struct {
	Server ServerConfig `yaml:"server"`
	Client ClientConfig `yaml:"client"`
	Tmux   TmuxConfig   `yaml:"tmux"`
}

// ServerConfig 服务器连接配置
type ServerConfig struct {
	URL               string `yaml:"url"`
	Token             string `yaml:"token"`
	ReconnectInterval int    `yaml:"reconnect_interval"`
}

// ClientConfig 客户端标识配置
type ClientConfig struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// TmuxConfig tmux 配置
type TmuxConfig struct {
	SessionName string `yaml:"session_name"`
	AutoStart   bool   `yaml:"auto_start"`
	Shell       string `yaml:"shell"` // 默认 shell
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			URL:               "wss://localhost:8765",
			ReconnectInterval: 5,
		},
		Client: ClientConfig{
			Name: "Claude Forward Client",
		},
		Tmux: TmuxConfig{
			SessionName: "claude-forward",
			AutoStart:   true,
		},
	}
}

// LoadConfig 从文件加载配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	config := DefaultConfig()
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, err
	}

	return config, nil
}
