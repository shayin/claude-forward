package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PermissionAction 权限动作
type PermissionAction string

const (
	ActionAllow PermissionAction = "allow"
	ActionDeny  PermissionAction = "deny"
	ActionAsk   PermissionAction = "ask"
)

// PermissionRule 权限规则
type PermissionRule struct {
	Tool      string         // 工具名，如 "Bash", "Read", "Edit"
	Pattern   string         // 参数匹配模式，如 "rm *", "./.env*"
	Action    PermissionAction // "allow", "deny", "ask"
	HasPattern bool           // 是否有参数匹配模式
}

// PermissionChecker 权限检查器
type PermissionChecker struct {
	rules []PermissionRule
}

// NewPermissionChecker 从 ~/.claude/settings.json 加载权限规则
func NewPermissionChecker() (*PermissionChecker, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// settings.json 不存在，返回空检查器（所有工具默认 allow）
			return &PermissionChecker{}, nil
		}
		return nil, fmt.Errorf("failed to read settings.json: %w", err)
	}

	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
			Ask   []string `json:"ask"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("failed to parse settings.json: %w", err)
	}

	var rules []PermissionRule

	for _, r := range settings.Permissions.Allow {
		rules = append(rules, parseRule(r, ActionAllow))
	}
	for _, r := range settings.Permissions.Deny {
		rules = append(rules, parseRule(r, ActionDeny))
	}
	for _, r := range settings.Permissions.Ask {
		rules = append(rules, parseRule(r, ActionAsk))
	}

	return &PermissionChecker{rules: rules}, nil
}

// parseRule 解析单条规则，如 "Bash(rm *)" → {Tool: "Bash", Pattern: "rm *", HasPattern: true}
func parseRule(rule string, action PermissionAction) PermissionRule {
	rule = strings.TrimSpace(rule)
	if idx := strings.Index(rule, "("); idx != -1 {
		tool := rule[:idx]
		pattern := strings.TrimSuffix(rule[idx+1:], ")")
		return PermissionRule{
			Tool:        tool,
			Pattern:     pattern,
			Action:      action,
			HasPattern:  true,
		}
	}
	return PermissionRule{
		Tool:   rule,
		Action: action,
	}
}

// Check 检查工具调用是否需要权限，返回 "allow"/"deny"/"ask"
func (pc *PermissionChecker) Check(toolName string, toolInput json.RawMessage) PermissionAction {
	for _, rule := range pc.rules {
		if rule.Tool != toolName {
			continue
		}
		if !rule.HasPattern {
			// 无参数模式，整个工具匹配
			return rule.Action
		}
		// 提取匹配字段
		value := extractMatchValue(toolName, toolInput)
		if value == "" {
			continue
		}
		if matchPattern(rule.Pattern, value) {
			return rule.Action
		}
	}
	// 未匹配任何规则，默认 allow（因为我们使用 bypassPermissions，hook 只拦截需要提示的工具）
	return ActionAllow
}

// extractMatchValue 从工具输入中提取用于匹配的值
func extractMatchValue(toolName string, toolInput json.RawMessage) string {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(toolInput, &input); err != nil {
		return ""
	}

	switch toolName {
	case "Bash":
		// Bash 工具匹配 command 字段
		if cmd, ok := input["command"]; ok {
			var cmdStr string
			if err := json.Unmarshal(cmd, &cmdStr); err == nil {
				return cmdStr
			}
		}
	default:
		// 文件相关工具匹配 file_path 字段
		if fp, ok := input["file_path"]; ok {
			var fpStr string
			if err := json.Unmarshal(fp, &fpStr); err == nil {
				return fpStr
			}
		}
	}
	return ""
}

// matchPattern 简单的 glob 匹配，支持 * 通配符
func matchPattern(pattern, value string) bool {
	// 将 glob 模式转换为正则表达式
	regexPattern := globToRegex(pattern)
	return strings.HasPrefix(value, regexPattern) || matchGlob(pattern, value)
}

// matchGlob 简单 glob 匹配
func matchGlob(pattern, value string) bool {
	// 使用 filepath.Match 进行匹配
	matched, err := path.Match(pattern, value)
	if err != nil {
		// 如果 filepath.Match 失败，使用简单的前缀匹配
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			return strings.HasPrefix(value, prefix)
		}
		return pattern == value
	}
	return matched
}

// globToRegex 简单转换（保留前缀）
func globToRegex(pattern string) string {
	return strings.TrimSuffix(pattern, "*")
}
