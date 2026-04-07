package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 微信桥接配置
type Config struct {
	Server  ServerConfig          `yaml:"server"`
	Routing map[string]string     `yaml:"routing"`  // 微信 userID → clawbot_id
}

// ServerConfig Claude Forward Server 配置
type ServerConfig struct {
	URL             string `yaml:"url"`
	Token           string `yaml:"token"`
	DefaultClawbot  string `yaml:"default_clawbot_id"` // 默认 clawbot_id（路由未匹配时使用）
}

// DefaultConfig 默认配置
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			URL: "https://localhost:6022",
		},
		Routing: make(map[string]string),
	}
}

// LoadConfig 加载配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// ResolveClawbotID 根据微信用户 ID 解析目标 clawbot_id
func (c *Config) ResolveClawbotID(wechatUserID string) string {
	if id, ok := c.Routing[wechatUserID]; ok {
		return id
	}
	return c.Server.DefaultClawbot
}
