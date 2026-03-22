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

	// 终端 I/O
	TypeInput  MessageType = "input"  // 终端输入
	TypeOutput MessageType = "output" // 终端输出
	TypeResize MessageType = "resize" // 终端尺寸变化

	// 状态管理
	TypeList   MessageType = "list"   // 列出客户端
	TypeStatus MessageType = "status" // 客户端状态更新

	// 心跳
	TypePing MessageType = "ping"
	TypePong MessageType = "pong"

	// 错误
	TypeError MessageType = "error"
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
