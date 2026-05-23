package protocol

import "encoding/json"

// MessageType 定义消息类型
type MessageType string

const (
	// 客户端注册
	TypeRegister MessageType = "register"
	TypeAck      MessageType = "ack"

	// 连接管理
	TypeAttach   MessageType = "attach"   // 请求连接到客户端
	TypeDetach   MessageType = "detach"   // 断开连接
	TypeAttached MessageType = "attached" // 连接成功通知
	TypeDetached MessageType = "detached" // 断开通知

	// 终端 I/O（旧模式，保留兼容）
	TypeInput  MessageType = "input"  // 终端输入
	TypeOutput MessageType = "output" // 终端输出
	TypeResize MessageType = "resize" // 终端尺寸变化

	// 聊天模式
	TypeChatInput   MessageType = "chat_input"   // 用户发送聊天消息
	TypeChatAck     MessageType = "chat_ack"     // 客户端确认收到聊天消息
	TypeChatMessage MessageType = "chat_message"  // Claude 结构化输出事件
	TypeChatReady   MessageType = "chat_ready"    // Claude 处理完毕，可接受新输入
	TypeChatError   MessageType = "chat_error"    // Claude 处理错误

	// 会话管理
	TypeKillSession MessageType = "kill_session" // 销毁 tmux 会话
	TypeNewSession  MessageType = "new_session"  // 开始新 Claude 会话
	TypeSessionInfo MessageType = "session_info"  // 会话元数据

	// 状态管理
	TypeList   MessageType = "list"   // 列出客户端
	TypeStatus MessageType = "status" // 客户端状态更新

	// 心跳
	TypePing MessageType = "ping"
	TypePong MessageType = "pong"

	// 权限审批
	TypePermissionRequest  MessageType = "permission_request"  // 客户端请求用户审批工具权限
	TypePermissionResponse MessageType = "permission_response" // 用户返回权限审批结果

	// 错误
	TypeError MessageType = "error"

	// 后台任务
	TypeBackgroundMode   MessageType = "background_mode"    // Server→Client: 当前任务转后台模式
	TypeBackgroundStart  MessageType = "background_start"   // Server→Client: 启动新后台任务
	TypeBackgroundResult MessageType = "background_result"   // Client→Server: 后台任务完成
)

// Message WebSocket 消息格式
type Message struct {
	Type    MessageType     `json:"type"`
	From    string          `json:"from,omitempty"` // 发送者 ID
	To      string          `json:"to,omitempty"`   // 接收者 ID
	Payload json.RawMessage `json:"payload,omitempty"`
}

// RegisterPayload 客户端注册载荷
type RegisterPayload struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ClawbotID   string `json:"clawbot_id,omitempty"`
	PID         int    `json:"pid"`
	Path        string `json:"path"`
}

// AttachPayload 连接请求载荷
type AttachPayload struct {
	ClientID string `json:"client_id"`
}

// InputPayload 终端输入载荷
type InputPayload struct {
	Data string `json:"data"`
}

// OutputPayload 终端输出载荷
type OutputPayload struct {
	Data string `json:"data"`
}

// ResizePayload 终端尺寸变化载荷
type ResizePayload struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// StatusPayload 客户端状态载荷
type StatusPayload struct {
	ClientID string `json:"client_id"`
	Online   bool   `json:"online"`
	Message  string `json:"message,omitempty"`
}

// ClientInfo 客户端信息
type ClientInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ClawbotID   string `json:"clawbot_id,omitempty"`
	PID         int    `json:"pid"`
	Path        string `json:"path"`
	Online      bool   `json:"online"`
	ConnectedAt int64  `json:"connected_at,omitempty"`
}

// ListPayload 客户端列表载荷
type ListPayload struct {
	Clients []ClientInfo `json:"clients"`
}

// ErrorPayload 错误载荷
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ChatInputPayload 聊天输入载荷
type ChatInputPayload struct {
	Text string `json:"text"`
}

// ChatMessagePayload 聊天消息载荷（Claude 输出事件）
type ChatMessagePayload struct {
	EventType  string          `json:"event_type"`            // "text", "thinking", "tool_start", "tool_end", "result", "stream_delta"
	Text       string          `json:"text,omitempty"`        // 文本内容
	ToolID     string          `json:"tool_id,omitempty"`     // 工具调用 ID
	ToolName   string          `json:"tool_name,omitempty"`   // 工具名称
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`  // 工具输入参数
	ToolOutput string          `json:"tool_output,omitempty"` // 工具输出
	CostUSD    float64         `json:"cost_usd,omitempty"`    // 费用
	IsPartial  bool            `json:"is_partial,omitempty"`  // 是否为流式片段
	SessionID  string          `json:"session_id,omitempty"`  // Claude 会话 ID
	// Token 用量
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ContextWindow            int `json:"context_window,omitempty"`
}

// SessionInfoPayload 会话元数据
type SessionInfoPayload struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
}

// PermissionRequestPayload 权限请求载荷
type PermissionRequestPayload struct {
	RequestID string          `json:"request_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Timestamp int64           `json:"timestamp"`
}

// PermissionResponsePayload 权限响应载荷
type PermissionResponsePayload struct {
	RequestID string `json:"request_id"`
	Approved  bool   `json:"approved"`
}

// BackgroundModePayload 后台模式通知（Server→Client）
type BackgroundModePayload struct {
	TaskID   string `json:"task_id"`
	WechatID string `json:"wechat_id"`
}

// BackgroundStartPayload 启动后台任务（Server→Client）
type BackgroundStartPayload struct {
	TaskID   string `json:"task_id"`
	Text     string `json:"text"`
	WechatID string `json:"wechat_id"`
}

// BackgroundResultPayload 后台任务结果（Client→Server）
type BackgroundResultPayload struct {
	TaskID    string  `json:"task_id"`
	WechatID  string  `json:"wechat_id"`
	FullText  string  `json:"full_text"`
	IsError   bool    `json:"is_error"`
	ErrorMsg  string  `json:"error_msg,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

// NewMessage 创建新消息
func NewMessage(t MessageType, payload any) (*Message, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Message{
		Type:    t,
		Payload: data,
	}, nil
}

// MustMarshal 序列化为 JSON，出错时 panic
func MustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// ParsePayload 解析消息载荷
func (m *Message) ParsePayload(v any) error {
	return json.Unmarshal(m.Payload, v)
}
