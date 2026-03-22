package server

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 服务器配置
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Auth     AuthConfig     `yaml:"auth"`
	Session  SessionConfig  `yaml:"session"`
	Logging  LoggingConfig  `yaml:"logging"`
}

// ServerConfig 服务器基础配置
type ServerConfig struct {
	Host string    `yaml:"host"`
	Port int       `yaml:"port"`
	TLS  TLSConfig `yaml:"tls"`
}

// TLSConfig TLS 配置
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// 域名模式（Let's Encrypt）
	Domain string `yaml:"domain"`
}

// AuthConfig 认证配置
type AuthConfig struct {
	Tokens []string `yaml:"tokens"`
}

// SessionConfig 会话配置
type SessionConfig struct {
	Timeout    int `yaml:"timeout"`     // 会话超时（秒）
	MaxClients int `yaml:"max_clients"` // 最大客户端数
}

// LoggingConfig 日志配置
type LoggingConfig struct {
	Enabled  bool   `yaml:"enabled"`
	File     string `yaml:"file"`
	MaxDays  int    `yaml:"max_days"`  // 日志保留天数
	LogLevel string `yaml:"log_level"` // debug, info, warn, error
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8765,
			TLS: TLSConfig{
				Enabled: true,
			},
		},
		Auth: AuthConfig{
			Tokens: []string{},
		},
		Session: SessionConfig{
			Timeout:    300,
			MaxClients: 10,
		},
		Logging: LoggingConfig{
			Enabled: true,
			File:    "/var/log/claude-forward/server.log",
			MaxDays: 7,
			LogLevel: "info",
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
