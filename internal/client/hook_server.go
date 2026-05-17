package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
)

// HookServer 接收 Claude Code PreToolUse Hook 调用
type HookServer struct {
	server   *http.Server
	listener net.Listener
	port     int
	checker  *PermissionChecker
	pending  map[string]chan bool // requestID → response channel
	mu       sync.RWMutex
	sendToUI func(msg *protocol.Message)
	timeout  time.Duration
	settings string // 生成的 settings 文件路径
	clientID string // 客户端唯一标识
}

// NewHookServer 创建并启动 Hook Server
func NewHookServer(checker *PermissionChecker, timeout time.Duration, sendToUI func(msg *protocol.Message), clientID string) (*HookServer, error) {
	hs := &HookServer{
		checker:  checker,
		pending:  make(map[string]chan bool),
		sendToUI: sendToUI,
		timeout:  timeout,
		clientID: clientID,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hook/pre-tool-use", hs.handlePreToolUse)

	hs.server = &http.Server{
		Handler: mux,
	}

	// 监听在随机端口
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}
	hs.listener = listener
	hs.port = listener.Addr().(*net.TCPAddr).Port

	// 在后台启动 HTTP 服务器
	go func() {
		if err := hs.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("Hook server error: %v", err)
		}
	}()

	// 生成 settings 文件
	settingsPath, err := hs.GenerateSettingsFile()
	if err != nil {
		hs.server.Close()
		return nil, fmt.Errorf("failed to generate settings file: %w", err)
	}
	hs.settings = settingsPath

	log.Printf("Hook server started on port %d", hs.port)
	return hs, nil
}

// Port 返回监听端口
func (hs *HookServer) Port() int {
	return hs.port
}

// SettingsPath 返回生成的 settings 文件路径
func (hs *HookServer) SettingsPath() string {
	return hs.settings
}

// HandleResponse 处理来自 Web UI 的权限审批结果
func (hs *HookServer) HandleResponse(requestID string, approved bool) {
	hs.mu.Lock()
	if ch, ok := hs.pending[requestID]; ok {
		log.Printf("[PERM] HandleResponse: found pending request %s, approved=%v", requestID, approved)
		ch <- approved
		delete(hs.pending, requestID)
	} else {
		log.Printf("[PERM] HandleResponse: requestID %s NOT FOUND in pending map (pending count=%d)", requestID, len(hs.pending))
	}
	hs.mu.Unlock()
}

// Close 关闭 Hook Server
func (hs *HookServer) Close() error {
	// 不删除持久化的 settings 文件，避免被 OS 清理后无法恢复
	if hs.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return hs.server.Shutdown(ctx)
	}
	return nil
}

// handlePreToolUse 处理 POST /hook/pre-tool-use
func (hs *HookServer) handlePreToolUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// 解析 Claude Hook 传入的数据
	var hookData struct {
		ToolName string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		SessionID string         `json:"session_id"`
	}
	if err := json.Unmarshal(body, &hookData); err != nil {
		// 无法解析，默认允许
		w.WriteHeader(http.StatusOK)
		return
	}

	// 检查权限规则
	action := hs.checker.Check(hookData.ToolName, hookData.ToolInput)
	switch action {
	case ActionAllow:
		w.WriteHeader(http.StatusOK)
		return
	case ActionDeny:
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "Permission denied by rule")
		return
	case ActionAsk:
		// 需要用户审批
		hs.handleAskPermission(w, hookData.ToolName, hookData.ToolInput)
		return
	}

	// 默认允许
	w.WriteHeader(http.StatusOK)
}

// handleAskPermission 处理需要用户审批的工具调用
func (hs *HookServer) handleAskPermission(w http.ResponseWriter, toolName string, toolInput json.RawMessage) {
	requestID := uuid.New().String()
	responseCh := make(chan bool, 1)

	// 注册等待通道
	hs.mu.Lock()
	hs.pending[requestID] = responseCh
	hs.mu.Unlock()

	// 清理
	defer func() {
		hs.mu.Lock()
		delete(hs.pending, requestID)
		hs.mu.Unlock()
	}()

	// 构建并发送权限请求到 Web UI
	msg, err := protocol.NewMessage(protocol.TypePermissionRequest, protocol.PermissionRequestPayload{
		RequestID: requestID,
		ToolName:  toolName,
		ToolInput: toolInput,
		Timestamp: time.Now().Unix(),
	})
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "Failed to create permission request")
		return
	}

	hs.sendToUI(msg)

	log.Printf("[PERM] Waiting for response to request %s (timeout=%v)", requestID, hs.timeout)

	// 等待用户响应或超时
	select {
	case approved := <-responseCh:
		log.Printf("[PERM] Received response for request %s: approved=%v", requestID, approved)
		if approved {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "Permission denied by user")
		}
	case <-time.After(hs.timeout):
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "Permission request timed out")
		log.Printf("[PERM] Request %s TIMED OUT for tool %s (pending count=%d)", requestID, toolName, len(hs.pending))
	}
}

// GenerateSettingsFile 生成包含 PreToolUse Hook 配置的临时 settings 文件
// 读取用户 ~/.claude/settings.json，保留 env 等关键配置，但覆盖 permissions.deny 为空
func (hs *HookServer) GenerateSettingsFile() (string, error) {
	hookCommand := fmt.Sprintf(
		`sh -c 'curl -sf -X POST http://127.0.0.1:%d/hook/pre-tool-use -d @- && exit 0 || (echo "Permission denied"; exit 2)'`,
		hs.port,
	)

	// 从用户 settings.json 读取关键配置（env、model 等）
	settings := hs.loadUserSettings()

	// 覆盖 permissions：全部允许，清空 deny
	settings["permissions"] = map[string]any{
		"allow": []string{
			"Bash",
			"Read",
			"Edit",
			"Write",
			"Glob",
			"Grep",
			"WebSearch",
			"WebFetch",
			"NotebookEdit",
			"LSP",
		},
		"ask":  []string{},
		"deny": []string{},
	}

	// 注入 PreToolUse Hook
	settings["hooks"] = map[string]any{
		"PreToolUse": []map[string]any{
			{
				"matcher": "",
				"hooks": []map[string]any{
					{
						"type":    "command",
						"command": hookCommand,
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal settings: %w", err)
	}

	// 使用持久化目录，避免被 macOS 清理临时文件
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home dir: %w", err)
	}
	settingsDir := filepath.Join(homeDir, ".claude-forward")
	os.MkdirAll(settingsDir, 0755)

	settingsPath := filepath.Join(settingsDir, "hooks-settings.json")
	if hs.clientID != "" {
		settingsPath = filepath.Join(settingsDir, fmt.Sprintf("hooks-settings-%s.json", hs.clientID))
	}
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write settings: %w", err)
	}

	log.Printf("Generated settings file: %s", settingsPath)
	return settingsPath, nil
}

// loadUserSettings 读取用户 ~/.claude/settings.json 中的关键配置
// 保留 env（认证、API地址、模型）、model、language 等配置
func (hs *HookServer) loadUserSettings() map[string]any {
	result := make(map[string]any)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Failed to get home dir: %v", err)
		return result
	}

	settingsPath := homeDir + "/.claude/settings.json"
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		log.Printf("Failed to read user settings: %v", err)
		return result
	}

	var userSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &userSettings); err != nil {
		log.Printf("Failed to parse user settings: %v", err)
		return result
	}

	// 保留关键配置项
	preserveKeys := []string{
		"env",                        // API 认证、Base URL、模型配置
		"model",                      // 默认模型
		"language",                   // 语言设置
		"apiProvider",                // API 提供商
		"mcpServers",                 // MCP 服务器配置
		"enabledPlugins",             // 插件配置（Skills 来源）
		"enableAllProjectMcpServers", // 启用项目级 MCP 服务器（插件 MCP 依赖）
	}

	for _, key := range preserveKeys {
		if raw, ok := userSettings[key]; ok {
			var value any
			if err := json.Unmarshal(raw, &value); err == nil {
				result[key] = value
			}
		}
	}

	log.Printf("Loaded user settings keys: %v", func() []string {
		keys := make([]string, 0, len(result))
		for k := range result {
			keys = append(keys, k)
		}
		return keys
	}())

	return result
}
