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
	WeChat   WeChatConfig   `yaml:"wechat"`
	Feishu   FeishuConfig   `yaml:"feishu"`
	// FeishuApps 多飞书应用列表（每个 app 独立机器人，路由到不同 clawbot）
	FeishuApps []FeishuConfig `yaml:"feishu_apps"`
}

// ServerConfig 服务器基础配置
type ServerConfig struct {
	Host          string    `yaml:"host"`
	Port          int       `yaml:"port"`
	EncryptionKey string    `yaml:"encryption_key"`
	TLS           TLSConfig `yaml:"tls"`
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
			Port: 6022,
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
		WeChat: WeChatConfig{
			DataDir: "wechat-data",
		},
		Feishu: FeishuConfig{
			DataDir: "feishu-data",
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

	// 单条 feishu（向后兼容）合并进 feishu_apps 统一处理
	config.FeishuApps = mergeFeishuApps(config.Feishu, config.FeishuApps)

	return config, nil
}

// mergeFeishuApps 把单条 feishu 配置插入 apps 列表头部，返回合并后的列表
func mergeFeishuApps(single FeishuConfig, apps []FeishuConfig) []FeishuConfig {
	if !single.Enabled {
		return apps
	}
	merged := make([]FeishuConfig, 0, len(apps)+1)
	merged = append(merged, single)
	merged = append(merged, apps...)
	return merged
}

// WeChatConfig 微信集成配置
type WeChatConfig struct {
	Enabled bool         `yaml:"enabled"`
	DataDir string       `yaml:"data_dir"`
	Users   []UserRoute  `yaml:"users"`
}

// UserRoute 微信用户 → 电脑 clawbot_id 路由
type UserRoute struct {
	WechatID   string `yaml:"wechat_id"`
	ClawbotID  string `yaml:"clawbot_id"`
	PushSecret string `yaml:"push_secret"`
}

// FeishuConfig 飞书集成配置
type FeishuConfig struct {
	Enabled   bool              `yaml:"enabled"`
	AppID     string            `yaml:"app_id"`
	AppSecret string            `yaml:"app_secret"`
	DataDir   string            `yaml:"data_dir"`
	Users     []FeishuUserRoute `yaml:"users"`
}

// FeishuUserRoute 飞书用户(open_id) → 电脑 clawbot_id 路由
type FeishuUserRoute struct {
	FeishuID   string `yaml:"feishu_id"`   // 飞书用户 open_id（白名单）
	ClawbotID  string `yaml:"clawbot_id"`  // 电脑级别 ID，对应 Client 的 clawbot_id
	PushSecret string `yaml:"push_secret"` // Push API 密钥（预留，飞书后台推送走 BackgroundResult 链路）
}
