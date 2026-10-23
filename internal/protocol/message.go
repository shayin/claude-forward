package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

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
	TypeChatMessage MessageType = "chat_message" // Claude 结构化输出事件
	TypeChatReady   MessageType = "chat_ready"   // Claude 处理完毕，可接受新输入
	TypeChatError   MessageType = "chat_error"   // Claude 处理错误

	// 会话管理
	TypeKillSession MessageType = "kill_session" // 销毁 tmux 会话
	TypeNewSession  MessageType = "new_session"  // 开始新 Claude 会话
	TypeSessionInfo MessageType = "session_info" // 会话元数据

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
	TypeBackgroundMode   MessageType = "background_mode"   // Server→Client: 当前任务转后台模式
	TypeBackgroundStart  MessageType = "background_start"  // Server→Client: 启动新后台任务
	TypeBackgroundResult MessageType = "background_result" // Client→Server: 后台任务完成
	TypeBackgroundAck    MessageType = "background_ack"    // Server→Client: 后台结果已接收

	// 配置管理
	TypeConfigUpdate MessageType = "config_update" // UI/Server→Client: 更新运行时配置
	TypeConfigInfo   MessageType = "config_info"   // Client→UI/Server: 返回当前配置信息

	// 本地 HTML 分享：Server 通过既有长连接向 Client 获取静态文件
	TypeFileRequest  MessageType = "file_request"
	TypeFileResponse MessageType = "file_response"

	// cmux 模式与原有聊天/旧终端链路隔离。
	TypeCmuxAttach    MessageType = "cmux_attach"
	TypeCmuxAttached  MessageType = "cmux_attached"
	TypeCmuxRequest   MessageType = "cmux_request"
	TypeCmuxResponse  MessageType = "cmux_response"
	TypeCmuxEvent     MessageType = "cmux_event"
	TypeCmuxClaim     MessageType = "cmux_claim"
	TypeCmuxRelease   MessageType = "cmux_release"
	TypeCmuxClaimed   MessageType = "cmux_claimed"
	TypeCmuxOwnership MessageType = "cmux_ownership" // Server → Client：租约代次变更
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
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	ClawbotID   string   `json:"clawbot_id,omitempty"`
	PID         int      `json:"pid"`
	Path        string   `json:"path"`
	Features    []string `json:"features,omitempty"`
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
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	ClawbotID   string   `json:"clawbot_id,omitempty"`
	PID         int      `json:"pid"`
	Path        string   `json:"path"`
	Features    []string `json:"features,omitempty"`
	Online      bool     `json:"online"`
	ConnectedAt int64    `json:"connected_at,omitempty"`
}

// CmuxAction 限定网页能够请求的操作，不能将任意 RPC 方法透传到本机。
type CmuxAction string

const (
	CmuxHealth            CmuxAction = "health"
	CmuxList              CmuxAction = "list"
	CmuxSnapshot          CmuxAction = "snapshot"
	CmuxText              CmuxAction = "text"
	CmuxKey               CmuxAction = "key"
	CmuxPaste             CmuxAction = "paste"
	CmuxWorkspaceRename   CmuxAction = "workspace_rename"
	CmuxWorkspaceCreate   CmuxAction = "workspace_create"
	CmuxWorkspaceClose    CmuxAction = "workspace_close"
	CmuxWorkspaceReorder  CmuxAction = "workspace_reorder"
	CmuxGroupCreate       CmuxAction = "group_create"
	CmuxGroupRename       CmuxAction = "group_rename"
	CmuxGroupAdd          CmuxAction = "group_add"
	CmuxGroupRemove       CmuxAction = "group_remove"
	CmuxTerminalCreate    CmuxAction = "terminal_create"
	CmuxTerminalClose     CmuxAction = "terminal_close"
	CmuxScroll            CmuxAction = "scroll"
	CmuxMouse             CmuxAction = "mouse"
	CmuxViewportReport    CmuxAction = "viewport_report"
	CmuxViewportClear     CmuxAction = "viewport_clear"
	CmuxNotifications     CmuxAction = "notifications"
	CmuxNotificationRead  CmuxAction = "notification_read"
	CmuxFileRoots         CmuxAction = "file_roots"
	CmuxFileList          CmuxAction = "file_list"
	CmuxFileRead          CmuxAction = "file_read"
	CmuxFileRecent        CmuxAction = "file_recent"
	CmuxUploadBegin       CmuxAction = "upload_begin"
	CmuxUploadChunk       CmuxAction = "upload_chunk"
	CmuxUploadFinish      CmuxAction = "upload_finish"
	CmuxUploadAbort       CmuxAction = "upload_abort"
	CmuxBrowserPorts      CmuxAction = "browser_ports"
	CmuxBrowserNavigate   CmuxAction = "browser_navigate"
	CmuxBrowserScreenshot CmuxAction = "browser_screenshot"
	CmuxBrowserClick      CmuxAction = "browser_click"
	CmuxBrowserType       CmuxAction = "browser_type"
	CmuxBrowserKey        CmuxAction = "browser_key"
	CmuxBrowserScroll     CmuxAction = "browser_scroll"
	CmuxBrowserNav        CmuxAction = "browser_nav"
)

type CmuxRequestPayload struct {
	RequestID      string     `json:"request_id"`
	Action         CmuxAction `json:"action"`
	WorkspaceID    string     `json:"workspace_id,omitempty"`
	SurfaceID      string     `json:"surface_id,omitempty"`
	GroupID        string     `json:"group_id,omitempty"`
	NotificationID string     `json:"notification_id,omitempty"`
	Title          string     `json:"title,omitempty"`
	Directory      string     `json:"directory,omitempty"`
	Profile        string     `json:"profile,omitempty"` // 空终端、claude、codex；不接收任意启动命令
	Text           string     `json:"text,omitempty"`
	Key            string     `json:"key,omitempty"`
	Index          int        `json:"index,omitempty"`
	DeltaLines     int        `json:"delta_lines,omitempty"`
	Column         int        `json:"column,omitempty"`
	Row            int        `json:"row,omitempty"`
	Columns        int        `json:"columns,omitempty"`
	Rows           int        `json:"rows,omitempty"`
	Generation     int        `json:"generation,omitempty"`
	ViewerID       string     `json:"viewer_id,omitempty"`        // Server 注入，忽略浏览器传值
	LeaseSurfaceID string     `json:"lease_surface_id,omitempty"` // Server 注入
	LeaseRevision  uint64     `json:"lease_revision,omitempty"`   // Server 注入
	Path           string     `json:"path,omitempty"`
	Query          string     `json:"query,omitempty"`
	Offset         int64      `json:"offset,omitempty"`
	ReadBytes      int        `json:"read_bytes,omitempty"`
	UploadID       string     `json:"upload_id,omitempty"`
	UploadKind     string     `json:"upload_kind,omitempty"` // attachment | file
	FileName       string     `json:"file_name,omitempty"`
	Size           int64      `json:"size,omitempty"`
	Data           []byte     `json:"data,omitempty"` // 256 KiB 分片，避免大 WebSocket 消息
	Port           int        `json:"port,omitempty"`
	BrowserPath    string     `json:"browser_path,omitempty"`
	X              float64    `json:"x,omitempty"` // 网页截图相对坐标 [0,1]
	Y              float64    `json:"y,omitempty"`
	ScrollY        int        `json:"scroll_y,omitempty"`
}

const MaxCmuxRequestPayloadBytes = 512 << 10
const CmuxAdminSurfaceID = "__cmux_admin"

func IsCmuxBrowserAction(action CmuxAction) bool {
	switch action {
	case CmuxBrowserPorts, CmuxBrowserNavigate, CmuxBrowserScreenshot,
		CmuxBrowserClick, CmuxBrowserType, CmuxBrowserKey,
		CmuxBrowserScroll, CmuxBrowserNav:
		return true
	}
	return false
}

// ValidateCmuxRequest 在云端与本机同时执行，拒绝过大的消息和任意 socket 方法调用。
func ValidateCmuxRequest(request CmuxRequestPayload) error {
	validID := func(value string) bool {
		return len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
	}
	if request.RequestID == "" || !validID(request.RequestID) ||
		!validID(request.WorkspaceID) || !validID(request.SurfaceID) ||
		!validID(request.GroupID) || !validID(request.NotificationID) ||
		!validID(request.UploadID) {
		return errors.New("invalid cmux request identifier")
	}
	validTitle := func(value string) bool {
		return value != "" && len(value) <= 256 && utf8.ValidString(value) &&
			!strings.ContainsAny(value, "\r\n\x00")
	}
	surface := request.SurfaceID != ""
	workspace := request.WorkspaceID != ""
	switch request.Action {
	case CmuxHealth, CmuxList, CmuxNotifications, CmuxFileRoots:
		return nil
	case CmuxFileList:
		if validFilePath(request.Path, true) && request.Offset >= 0 && request.Offset <= 100000 &&
			len(request.Query) <= 128 && utf8.ValidString(request.Query) &&
			!strings.ContainsAny(request.Query, "\x00\r\n") {
			return nil
		}
	case CmuxFileRead:
		if validFilePath(request.Path, false) && request.Offset >= 0 && request.Offset <= 1<<40 &&
			request.ReadBytes > 0 && request.ReadBytes <= 256<<10 {
			return nil
		}
	case CmuxFileRecent:
		if validFilePath(request.Path, true) {
			return nil
		}
	case CmuxBrowserPorts, CmuxBrowserScreenshot:
		if workspace {
			return nil
		}
	case CmuxBrowserNavigate:
		if workspace && request.Port > 0 && request.Port <= 65535 &&
			len(request.BrowserPath) <= 2048 && utf8.ValidString(request.BrowserPath) &&
			(request.BrowserPath == "" || strings.HasPrefix(request.BrowserPath, "/")) &&
			!strings.ContainsAny(request.BrowserPath, "\x00\r\n\\#") {
			return nil
		}
	case CmuxBrowserClick:
		if workspace && request.X >= 0 && request.X <= 1 &&
			request.Y >= 0 && request.Y <= 1 {
			return nil
		}
	case CmuxBrowserType:
		if workspace && request.Text != "" && len(request.Text) <= 4096 &&
			utf8.ValidString(request.Text) && !strings.ContainsRune(request.Text, 0) {
			return nil
		}
	case CmuxBrowserKey:
		if workspace && (request.Key == "Enter" || request.Key == "Escape" ||
			request.Key == "Tab" || request.Key == "Backspace" ||
			request.Key == "ArrowUp" || request.Key == "ArrowDown") {
			return nil
		}
	case CmuxBrowserScroll:
		if workspace && request.ScrollY >= -2000 && request.ScrollY <= 2000 &&
			request.ScrollY != 0 {
			return nil
		}
	case CmuxBrowserNav:
		if workspace && (request.Key == "back" || request.Key == "forward" || request.Key == "reload") {
			return nil
		}
	case CmuxUploadBegin:
		if validUploadKind(request) && validUploadName(request.FileName) &&
			request.Size > 0 && request.Size <= 20<<20 &&
			(request.UploadKind == "attachment" && workspace && surface ||
				request.UploadKind == "file" && validFilePath(request.Path, false)) {
			return nil
		}
	case CmuxUploadChunk:
		if validUploadKind(request) && request.UploadID != "" &&
			request.Offset >= 0 && request.Offset <= 20<<20 &&
			len(request.Data) > 0 && len(request.Data) <= 256<<10 {
			return nil
		}
	case CmuxUploadFinish, CmuxUploadAbort:
		if validUploadKind(request) && request.UploadID != "" {
			return nil
		}
	case CmuxSnapshot:
		if surface {
			return nil
		}
	case CmuxText, CmuxPaste:
		if surface && request.Text != "" && len(request.Text) <= 64<<10 && utf8.ValidString(request.Text) {
			return nil
		}
	case CmuxKey:
		if surface && request.Key != "" && len(request.Key) <= 32 && validID(request.Key) {
			return nil
		}
	case CmuxWorkspaceRename:
		if workspace && validTitle(request.Title) {
			return nil
		}
	case CmuxWorkspaceCreate:
		if validTitle(request.Title) && request.Directory != "" &&
			len(request.Directory) <= 4096 && utf8.ValidString(request.Directory) &&
			!strings.ContainsAny(request.Directory, "\x00\r\n") &&
			(request.Profile == "" || request.Profile == "claude" || request.Profile == "codex") {
			return nil
		}
	case CmuxWorkspaceClose:
		if workspace {
			return nil
		}
	case CmuxWorkspaceReorder:
		if workspace && request.Index >= 0 && request.Index < 1000 {
			return nil
		}
	case CmuxGroupCreate:
		if validTitle(request.Title) && len(request.Directory) <= 4096 &&
			utf8.ValidString(request.Directory) && !strings.ContainsAny(request.Directory, "\x00\r\n") {
			return nil
		}
	case CmuxGroupRename:
		if request.GroupID != "" && validTitle(request.Title) {
			return nil
		}
	case CmuxGroupAdd:
		if workspace && request.GroupID != "" {
			return nil
		}
	case CmuxGroupRemove:
		if workspace {
			return nil
		}
	case CmuxTerminalCreate:
		if workspace {
			return nil
		}
	case CmuxTerminalClose:
		if workspace && surface {
			return nil
		}
	case CmuxScroll:
		if workspace && surface && request.DeltaLines != 0 &&
			request.DeltaLines >= -1000 && request.DeltaLines <= 1000 {
			return nil
		}
	case CmuxMouse:
		if workspace && surface && request.Column >= 0 && request.Column < 1000 &&
			request.Row >= 0 && request.Row < 1000 {
			return nil
		}
	case CmuxViewportReport:
		if surface && request.Columns >= 20 && request.Columns <= 300 &&
			request.Rows >= 5 && request.Rows <= 120 &&
			request.Generation > 0 && request.Generation <= 1<<30 {
			return nil
		}
	case CmuxViewportClear:
		if surface && request.Generation > 0 && request.Generation <= 1<<30 {
			return nil
		}
	case CmuxNotificationRead:
		if request.NotificationID != "" {
			return nil
		}
	default:
		return errors.New("unsupported cmux action")
	}
	return errors.New("invalid cmux request parameters")
}

func validFilePath(path string, optional bool) bool {
	return (optional && path == "" || path != "" && strings.HasPrefix(path, "/")) &&
		len(path) <= 4096 && utf8.ValidString(path) &&
		!strings.ContainsAny(path, "\x00\r\n")
}

func validUploadKind(request CmuxRequestPayload) bool {
	if request.UploadKind == "file" {
		return true
	}
	return request.UploadKind == "attachment" &&
		request.WorkspaceID != "" && request.SurfaceID != ""
}

func validUploadName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		len(name) <= 255 && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\\\x00\r\n") &&
		!strings.ContainsAny(name, "\t\x7f")
}

func ValidCmuxSurfaceID(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\r\n\x00")
}

type CmuxResponsePayload struct {
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// CmuxEventPayload 只传失效信号和有限程序状态；终端正文/Hook 负载不进入事件广播。
type CmuxEventPayload struct {
	Sequence    uint64              `json:"sequence,omitempty"`
	Category    string              `json:"category,omitempty"`
	Name        string              `json:"name,omitempty"`
	WorkspaceID string              `json:"workspace_id,omitempty"`
	SurfaceID   string              `json:"surface_id,omitempty"`
	Resync      bool                `json:"resync,omitempty"`
	Gap         bool                `json:"gap,omitempty"`
	Programs    []CmuxProgramRecord `json:"programs,omitempty"`
}

// 只包含程序进度的有限元数据，不转发 Hook/终端事件正文。
type CmuxProgramRecord struct {
	ID       string `json:"id,omitempty"`
	State    string `json:"state"`
	Progress *int   `json:"progress,omitempty"`
	App      string `json:"app,omitempty"`
	Title    string `json:"title,omitempty"`
	Message  string `json:"msg,omitempty"`
}

// CleanCmuxPrograms 在 Client 及 Server 两个信任边界执行相同的字段白名单/长度限制。
func CleanCmuxPrograms(records []CmuxProgramRecord) []CmuxProgramRecord {
	clean := make([]CmuxProgramRecord, 0, min(len(records), 16))
	text := func(value string, limit int) string {
		if !utf8.ValidString(value) {
			return ""
		}
		var builder strings.Builder
		for _, character := range value {
			if character < 32 || character == 127 {
				continue
			}
			if builder.Len()+utf8.RuneLen(character) > limit {
				break
			}
			builder.WriteRune(character)
		}
		return builder.String()
	}
	for _, record := range records {
		switch record.State {
		case "working", "blocked", "done", "error", "idle":
		default:
			continue
		}
		if record.Progress != nil && (*record.Progress < 0 || *record.Progress > 100) {
			record.Progress = nil
		}
		record.ID = text(record.ID, 128)
		record.App = text(record.App, 128)
		record.Title = text(record.Title, 256)
		record.Message = text(record.Message, 512)
		clean = append(clean, record)
		if len(clean) == 16 {
			break
		}
	}
	return clean
}

// CmuxClaimPayload 的 Force 仅用于用户明确点选“接管”时；普通选中不会抢占另一设备。
type CmuxClaimPayload struct {
	SurfaceID string `json:"surface_id"`
	Force     bool   `json:"force,omitempty"`
}

// CmuxClaimedPayload 不包含其他 Web 连接的身份信息。
type CmuxClaimedPayload struct {
	ClientID  string `json:"client_id"`
	SurfaceID string `json:"surface_id"`
	Owned     bool   `json:"owned"`
	Occupied  bool   `json:"occupied"`
}

// CmuxOwnershipPayload 仅由 Server 发送给本机 Client，用于废弃接管前的排队输入。
type CmuxOwnershipPayload struct {
	SurfaceID string `json:"surface_id"`
	ViewerID  string `json:"viewer_id,omitempty"`
	Revision  uint64 `json:"revision"`
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
	Text     string `json:"text"`
	WechatID string `json:"wechat_id,omitempty"` // 微信用户 ID（Server 端 chatViaHub 下发，供 Client 自动后台时使用）
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
	CreatedAt int64   `json:"created_at,omitempty"` // 生成时间(UnixMilli)，server 据此做时效校验
	IsResend  bool    `json:"is_resend,omitempty"`  // 是否为重连后重发（用于日志与展示；仍受时效校验）
}

// BackgroundAckPayload 确认服务端已接收（或已去重）后台结果，客户端可删除本地待发文件。
type BackgroundAckPayload struct {
	TaskID string `json:"task_id"`
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

// ConfigUpdatePayload 配置更新载荷（UI/Server→Client）
type ConfigUpdatePayload struct {
	EnvFile string `json:"env_file,omitempty"` // 新的 env_file 路径
}

// ConfigInfoPayload 当前配置信息（Client→UI/Server）
type ConfigInfoPayload struct {
	EnvFile   string   `json:"env_file"`            // 当前 env_file 路径
	Providers []string `json:"providers,omitempty"` // 可用 provider 列表
	Error     string   `json:"error,omitempty"`     // 错误信息
}

// FileRequestPayload 是 Server 向本地 Client 请求分享文件的载荷。
// Token 只在 Client 本地验证，避免服务端持久化 HTML 分享凭据。
type FileRequestPayload struct {
	RequestID string `json:"request_id"`
	Token     string `json:"token"`
	Path      string `json:"path"`
}

// FileResponsePayload 是 Client 返回给 Server 的分享文件结果。
// Content 会由 JSON 编码为 base64；服务端收到后原样写回 HTTP 响应。
type FileResponsePayload struct {
	RequestID   string `json:"request_id"`
	Path        string `json:"path"`
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type,omitempty"`
	Content     []byte `json:"content,omitempty"`
}
