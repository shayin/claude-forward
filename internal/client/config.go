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
	Server    ServerConfig    `yaml:"server"`
	Client    ClientConfig    `yaml:"client"`
	Tmux      TmuxConfig      `yaml:"tmux"`
	Claude    ClaudeConfig    `yaml:"claude"`
	Codex     CodexConfig     `yaml:"codex"`
	HTMLShare HTMLShareConfig `yaml:"html_share"`
	Path      string          // 工作目录（运行时推导，不来自配置文件）
}

// HTMLShareConfig 配置经云端反向代理对外分享的本地静态目录。
// RootDir 留空时功能关闭。
type HTMLShareConfig struct {
	RootDir       string `yaml:"root_dir"`
	PublicBaseURL string `yaml:"public_base_url"`
	TokenFile     string `yaml:"token_file"`
}

// ServerConfig 服务器连接配置
type ServerConfig struct {
	URL               string `yaml:"url"`
	Token             string `yaml:"token"`
	EncryptionKey     string `yaml:"encryption_key"`
	ReconnectInterval int    `yaml:"reconnect_interval"`
}

// ClientConfig 客户端标识配置
type ClientConfig struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	ClawbotID   string `yaml:"clawbot_id"` // 电脑级别 ID，用于微信路由
}

// TmuxConfig tmux 配置
type TmuxConfig struct {
	SessionName string `yaml:"session_name"`
	AutoStart   bool   `yaml:"auto_start"`
	Shell       string `yaml:"shell"` // 默认 shell
}

// CodexConfig Codex CLI 引擎配置（微信通道通过 /engine codex 启用，Web UI 不使用）
type CodexConfig struct {
	Path     string `yaml:"path"`     // codex 二进制路径，默认 "codex"
	Sandbox  string `yaml:"sandbox"`  // sandbox 模式: workspace-write | read-only | dangerously-bypass-approvals-and-sandbox
	Model    string `yaml:"model"`    // 可选模型覆盖（留空用 codex 默认模型）
	WorkDir  string `yaml:"work_dir"` // 可选工作目录（留空用 CWD，通常与 cc 一致）
	ClientID string `yaml:"-"`        // 客户端标识（运行时注入，用于区分多客户端 session 文件）
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
		Codex: CodexConfig{
			Path:    "codex",
			Sandbox: "workspace-write",
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
	if config.Client.ClawbotID == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "unknown"
		}
		config.Client.ClawbotID = hostname
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

// ResolveConfigPath 按优先级查找配置文件路径
// 1. 显式指定路径（非空则直接使用）
// 2. ~/.claude-forward/client.yaml（全局默认）
// 3. configs/client.yaml（CWD 下，向后兼容）
// 4. 都没有则返回空字符串，由调用方使用 DefaultConfig
func ResolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	homeDir, err := os.UserHomeDir()
	if err == nil && homeDir != "" {
		globalPath := filepath.Join(homeDir, ".claude-forward", "client.yaml")
		if _, err := os.Stat(globalPath); err == nil {
			return globalPath
		}
	}
	localPath := "configs/client.yaml"
	if _, err := os.Stat(localPath); err == nil {
		return localPath
	}
	return ""
}

// GenerateClientID 生成客户端 ID：hostname-basename
func GenerateClientID(basename string) string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return fmt.Sprintf("%s-%s", hostname, basename)
}
