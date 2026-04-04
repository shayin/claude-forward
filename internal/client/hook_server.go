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
}

// NewHookServer 创建并启动 Hook Server
func NewHookServer(checker *PermissionChecker, timeout time.Duration, sendToUI func(msg *protocol.Message)) (*HookServer, error) {
	hs := &HookServer{
		checker: checker,
		pending: make(map[string]chan bool),
		sendToUI: sendToUI,
		timeout: timeout,
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

// Close 关闭 Hook Server 并清理临时文件
func (hs *HookServer) Close() error {
	if hs.settings != "" {
		os.Remove(hs.settings)
	}
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
func (hs *HookServer) GenerateSettingsFile() (string, error) {
	hookCommand := fmt.Sprintf(
		`sh -c 'curl -sf -X POST http://127.0.0.1:%d/hook/pre-tool-use -d @- && exit 0 || (echo "Permission denied"; exit 2)'`,
		hs.port,
	)

	settings := map[string]any{
		"hooks": map[string]any{
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
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal settings: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "claude-forward-hooks-*.json")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer tmpFile.Close()

	if _, err := tmpFile.Write(data); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to write settings: %w", err)
	}

	return tmpFile.Name(), nil
}
