package client

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 客户端配置
type Config struct {
	Server ServerConfig `yaml:"server"`
	Client ClientConfig `yaml:"client"`
	Tmux   TmuxConfig   `yaml:"tmux"`
	Path   string       // 工作目录（运行时推导，不来自配置文件）
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

// DefaultConfig 返回默认配置（ID/Name/SessionName 留空，由 ApplyDefaults 动态推导）
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			URL:               "wss://localhost:6022",
			ReconnectInterval: 5,
		},
		Tmux: TmuxConfig{
			AutoStart: true,
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

	ApplyDefaults(config)
	return config, nil
}

// ApplyDefaults 对配置中空值字段用工作目录推导填充
func ApplyDefaults(config *Config) {
	sessionName, basename, absPath := DeriveFromPath(".")

	if config.Tmux.SessionName == "" {
		config.Tmux.SessionName = sessionName
	}
	if config.Client.ID == "" {
		config.Client.ID = GenerateClientID(basename)
	}
	if config.Client.Name == "" {
		config.Client.Name = basename
	}
	if config.Path == "" {
		config.Path = absPath
	}
}

// sanitizeTmuxName 清洗 tmux 会话名：`.` 和 `:` 替换为 `-`，折叠重复 `-`，截断 50 字符
func sanitizeTmuxName(name string) string {
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	re := regexp.MustCompile(`-{2,}`)
	name = re.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 50 {
		name = name[:50]
	}
	return name
}

// DeriveFromPath 从项目路径推导命名，返回 (tmuxSessionName, basename, absPath)
func DeriveFromPath(projectPath string) (string, string, string) {
	absPath, err := filepath.Abs(projectPath)
	if err != nil {
		absPath = projectPath
	}
	basename := filepath.Base(absPath)
	sessionName := fmt.Sprintf("cf-%s", sanitizeTmuxName(basename))
	return sessionName, basename, absPath
}

// GenerateClientID 生成客户端 ID：hostname-basename
func GenerateClientID(basename string) string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return fmt.Sprintf("%s-%s", hostname, basename)
}
