package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ClaudeEventType Claude 事件类型
type ClaudeEventType string

const (
	EventInit        ClaudeEventType = "init"
	EventText        ClaudeEventType = "text"
	EventThinking    ClaudeEventType = "thinking"
	EventToolStart   ClaudeEventType = "tool_start"
	EventToolEnd     ClaudeEventType = "tool_end"
	EventResult      ClaudeEventType = "result"
	EventStreamDelta ClaudeEventType = "stream_delta"
	EventError       ClaudeEventType = "error"
)

// ClaudeEvent Claude 输出事件
type ClaudeEvent struct {
	Type       ClaudeEventType   `json:"type"`
	SessionID  string            `json:"session_id,omitempty"`
	Text       string            `json:"text,omitempty"`
	ToolID     string            `json:"tool_id,omitempty"`
	ToolName   string            `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage   `json:"tool_input,omitempty"`
	ToolOutput string            `json:"tool_output,omitempty"`
	CostUSD    float64           `json:"cost_usd,omitempty"`
	IsPartial  bool              `json:"is_partial,omitempty"`
	// Token 用量（来自 result 事件）
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ContextWindow            int `json:"context_window,omitempty"`
}

// ClaudeManager 管理 Claude CLI 调用
type ClaudeManager struct {
	config    ClaudeConfig
	sessionID string     // Claude 会话 ID，用于 --resume
	cmd       *exec.Cmd  // 当前运行的 Claude 进程
	cancel    context.CancelFunc
	events    chan ClaudeEvent
	mu        sync.Mutex
	running   bool
}

// ClaudeConfig Claude 相关配置
type ClaudeConfig struct {
	Path             string `yaml:"path"`                          // claude 二进制路径，默认 "claude"
	AllowedTools     string `yaml:"allowed_tools"`                 // 允许的工具列表
	MaxTurns         int    `yaml:"max_turns"`                     // 最大轮次
	HookSettingsPath string `yaml:"-"` // Hook 配置文件路径（程序设置，不从 YAML 读取）
	ClientID         string `yaml:"-"` // 客户端唯一标识（用于区分多客户端文件路径）
}

// NewClaudeManager 创建 Claude 管理器
func NewClaudeManager(config ClaudeConfig) *ClaudeManager {
	if config.Path == "" {
		config.Path = "claude"
	}
	cm := &ClaudeManager{
		config:  config,
		events:  make(chan ClaudeEvent, 256),
		running: false,
	}
	// 从文件恢复上次的 session_id
	cm.loadSessionID()
	return cm
}

// SetHookSettingsPath 设置 Hook 配置文件路径
func (cm *ClaudeManager) SetHookSettingsPath(path string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.config.HookSettingsPath = path
}

// SessionID 返回当前会话 ID
func (cm *ClaudeManager) SessionID() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.sessionID
}

// SetSessionID 设置会话 ID
func (cm *ClaudeManager) SetSessionID(id string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.sessionID = id
	cm.saveSessionID()
}

// IsRunning 是否正在运行
func (cm *ClaudeManager) IsRunning() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.running
}

// SendMessage 发送消息给 Claude，启动一个 claude -p 子进程
func (cm *ClaudeManager) SendMessage(text string) error {
	cm.mu.Lock()
	if cm.running {
		cm.mu.Unlock()
		return fmt.Errorf("claude is still processing")
	}
	cm.running = true
	cm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cm.cancel = cancel

	// 构建命令参数
	args := []string{"-p", text, "--output-format", "stream-json", "--verbose"}

	cm.mu.Lock()
	if cm.sessionID != "" {
		args = append(args, "--resume", cm.sessionID)
	}
	cm.mu.Unlock()

	if cm.config.AllowedTools != "" {
		args = append(args, "--allowedTools", cm.config.AllowedTools)
	}
	// 绕过 Claude 内部权限系统：
	// 1. --dangerously-skip-permissions: 跳过所有权限检查
	// 2. --setting-sources "": 不加载用户 settings.json 中的 deny 规则
	args = append(args, "--dangerously-skip-permissions")
	args = append(args, "--setting-sources", "")
	if cm.config.HookSettingsPath != "" {
		args = append(args, "--settings", cm.config.HookSettingsPath)
	}
	if cm.config.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", cm.config.MaxTurns))
	}

	cmd := exec.CommandContext(ctx, cm.config.Path, args...)
	cmd.Dir = cm.getWorkDir()

	// 日志重定向到文件（按 clientID 区分），避免干扰
	logName := "claude-forward-claude.log"
	if cm.config.ClientID != "" {
		logName = fmt.Sprintf("claude-forward-claude-%s.log", cm.config.ClientID)
	}
	logFile, err := os.OpenFile(filepath.Join(os.TempDir(), logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stderr = logFile
	}

	// 获取 stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cm.setRunning(false)
		cancel()
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	cm.cmd = cmd

	if err := cmd.Start(); err != nil {
		cm.setRunning(false)
		cancel()
		return fmt.Errorf("failed to start claude: %w", err)
	}

	log.Printf("Claude process started: pid=%d args=%v", cmd.Process.Pid, args)

	// 启动 JSONL 读取协程
	go func() {
		defer func() {
			cm.setRunning(false)
			cancel()
			if logFile != nil {
				logFile.Close()
			}
		}()

		scanner := bufio.NewScanner(stdout)
		// 增大 buffer，Claude 的输出行可能很长
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			events := parseJSONLLine(line)
			for _, event := range events {
				// 捕获 session_id
				if event.SessionID != "" {
					cm.SetSessionID(event.SessionID)
				}

				select {
				case cm.events <- event:
				case <-ctx.Done():
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			log.Printf("Claude stdout scanner error: %v", err)
			cm.events <- ClaudeEvent{Type: EventError, Text: err.Error()}
		}

		// 等待进程结束
		if err := cmd.Wait(); err != nil {
			log.Printf("Claude process exited with error: %v", err)
		}
	}()

	return nil
}

// Stream 返回事件 channel
func (cm *ClaudeManager) Stream() <-chan ClaudeEvent {
	return cm.events
}

// Abort 终止当前 Claude 进程
func (cm *ClaudeManager) Abort() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.cancel != nil {
		cm.cancel()
		cm.cancel = nil
	}
	cm.running = false
}

// ResetSession 重置会话（开始新会话）
func (cm *ClaudeManager) ResetSession() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.sessionID = ""
	cm.saveSessionID()
}

func (cm *ClaudeManager) setRunning(r bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.running = r
}

func (cm *ClaudeManager) getWorkDir() string {
	// 使用当前工作目录
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

// sessionIDPath 返回 session_id 持久化文件路径（按 clientID 区分）
func (cm *ClaudeManager) sessionIDPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	name := "session_id"
	if cm.config.ClientID != "" {
		name = fmt.Sprintf("session_id_%s", cm.config.ClientID)
	}
	return filepath.Join(dir, ".claude-forward", name)
}

// saveSessionID 将 session_id 写入文件（调用方需持有 cm.mu）
func (cm *ClaudeManager) saveSessionID() {
	path := cm.sessionIDPath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	if cm.sessionID == "" {
		os.Remove(path)
	} else {
		os.WriteFile(path, []byte(cm.sessionID), 0644)
	}
}

// loadSessionID 从文件恢复 session_id
func (cm *ClaudeManager) loadSessionID() {
	path := cm.sessionIDPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	id := strings.TrimSpace(string(data))
	if id != "" {
		cm.sessionID = id
		log.Printf("Restored session_id from file: %s", id)
	}
}

// parseJSONLLine 解析单行 JSONL 为 ClaudeEvent(s)
// 一行可能产生多个事件（如 assistant 消息中包含多个 content block）
func parseJSONLLine(line string) []ClaudeEvent {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	var typeStr string
	if t, ok := raw["type"]; ok {
		json.Unmarshal(t, &typeStr)
	}

	var events []ClaudeEvent

	switch typeStr {
	case "system":
		// system 消息，可能包含 session init
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			if msg.Subtype == "init" && msg.SessionID != "" {
				events = append(events, ClaudeEvent{
					Type:      EventInit,
					SessionID: msg.SessionID,
				})
			}
		}

	case "assistant":
		// assistant 消息，包含 content blocks
		events = parseAssistantMessage(line)

	case "result":
		// 最终结果
		var msg struct {
			Type      string  `json:"type"`
			Result    string  `json:"result"`
			SessionID string  `json:"session_id"`
			CostUSD   float64 `json:"total_cost_usd"`
			Usage     struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
			ModelUsage map[string]struct {
				ContextWindow int `json:"contextWindow"`
			} `json:"modelUsage"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			evt := ClaudeEvent{
				Type:                    EventResult,
				Text:                    msg.Result,
				SessionID:               msg.SessionID,
				CostUSD:                 msg.CostUSD,
				InputTokens:             msg.Usage.InputTokens,
				OutputTokens:            msg.Usage.OutputTokens,
				CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:    msg.Usage.CacheReadInputTokens,
			}
			// 从 modelUsage 提取 contextWindow
			for _, m := range msg.ModelUsage {
				evt.ContextWindow = m.ContextWindow
				break
			}
			events = append(events, evt)
		}

	default:
		// 尝试解析为 content_block_delta（流式事件）
		if strings.HasPrefix(typeStr, "content_block") {
			events = parseStreamEvent(line, typeStr)
		}
	}

	return events
}

// parseAssistantMessage 解析 assistant 消息中的 content blocks
func parseAssistantMessage(line string) []ClaudeEvent {
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text,omitempty"`
				ID    string          `json:"id,omitempty"`
				Name  string          `json:"name,omitempty"`
				Input json.RawMessage `json:"input,omitempty"`
			} `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return nil
	}

	var events []ClaudeEvent
	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			events = append(events, ClaudeEvent{
				Type: EventText,
				Text: block.Text,
			})
		case "thinking":
			events = append(events, ClaudeEvent{
				Type: EventThinking,
				Text: block.Text,
			})
		case "tool_use":
			events = append(events, ClaudeEvent{
				Type:      EventToolStart,
				ToolID:    block.ID,
				ToolName:  block.Name,
				ToolInput: block.Input,
			})
		}
	}
	return events
}

// parseStreamEvent 解析流式事件（content_block_delta 等）
func parseStreamEvent(line, typeStr string) []ClaudeEvent {
	if typeStr == "content_block_delta" {
		var msg struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil && msg.Delta.Text != "" {
			return []ClaudeEvent{{
				Type:      EventStreamDelta,
				Text:      msg.Delta.Text,
				IsPartial: true,
			}}
		}
	}

	// content_block_start - 可能是 tool_use 开始
	if typeStr == "content_block_start" {
		var msg struct {
			Type          string `json:"type"`
			Index         int    `json:"index"`
			ContentBlock  struct {
				Type  string          `json:"type"`
				ID    string          `json:"id,omitempty"`
				Name  string          `json:"name,omitempty"`
				Input json.RawMessage `json:"input,omitempty"`
				Text  string          `json:"text,omitempty"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			switch msg.ContentBlock.Type {
			case "tool_use":
				return []ClaudeEvent{{
					Type:      EventToolStart,
					ToolID:    msg.ContentBlock.ID,
					ToolName:  msg.ContentBlock.Name,
					ToolInput: msg.ContentBlock.Input,
				}}
			case "text":
				if msg.ContentBlock.Text != "" {
					return []ClaudeEvent{{
						Type:      EventStreamDelta,
						Text:      msg.ContentBlock.Text,
						IsPartial: true,
					}}
				}
			}
		}
	}

	// content_block_stop - tool_use 结束
	if typeStr == "content_block_stop" {
		// 我们暂时不知道这个 block 的具体内容
		// 但可以发送一个通用的 end 事件
		return []ClaudeEvent{{
			Type: EventToolEnd,
		}}
	}

	return nil
}
