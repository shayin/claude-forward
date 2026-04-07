package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 微信桥接配置
type Config struct {
	Server  ServerConfig      `yaml:"server"`
	Routing map[string]string `yaml:"routing"` // 白名单：微信 userID → clawbot_id（不在列表内的用户拒绝连接）
}

// ServerConfig Claude Forward Server 配置
type ServerConfig struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// DefaultConfig 默认配置
func DefaultConfig() *Config {
	return &Config{
		Server:  ServerConfig{URL: "https://localhost:6022"},
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

// ResolveClawbotID 白名单路由：不在 routing 中的用户返回错误
func (c *Config) ResolveClawbotID(wechatUserID string) (string, error) {
	if id, ok := c.Routing[wechatUserID]; ok {
		return id, nil
	}
	return "", fmt.Errorf("用户 %s 不在白名单中", wechatUserID)
}
