package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shayin/claude-forward/internal/protocol"
)

// Client 客户端
type Client struct {
	config        *Config
	conn          *websocket.Conn
	send          chan *protocol.Message
	tmux          *TmuxManager
	claude        *ClaudeManager
	hookServer    *HookServer // 权限 Hook 服务器
	attachedUser  string      // 当前连接的用户 ID
	userMu        sync.RWMutex
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	forwardCancel context.CancelFunc // 用于停止 forwarding goroutine
	forwardMu     sync.Mutex
	connClosed    int32 // 连接是否已关闭的标志（用于防止重复关闭）

	// 会话级别事件缓冲：存储当前 Claude 会话的所有事件
	// 在 handleChatInput goroutine 退出后仍保留，支持断线重连后完整回放
	sessionEvents []protocol.Message
	sentUpTo      int         // 已发送到用户的事件索引（用于去重）
	sessionMu     sync.Mutex  // 保护 sessionEvents 和 sentUpTo
}

// NewClient 创建客户端
func NewClient(config *Config) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		config: config,
		send:   make(chan *protocol.Message, 256),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Connect 连接到服务器
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 重置连接关闭标志（用于重连场景）
	atomic.StoreInt32(&c.connClosed, 0)

	// 构建 WebSocket URL
	url := c.config.Server.URL
	if url == "" {
		url = "wss://localhost:6022"
	}

	// 添加 token
	dialer := websocket.DefaultDialer
	header := make(map[string][]string)
	header["Authorization"] = []string{"Bearer " + c.config.Server.Token}

	conn, _, err := dialer.Dial(url+"/ws?token="+c.config.Server.Token, header)
	if err != nil {
		return err
	}

	c.conn = conn

	// 注册
	registerMsg, _ := protocol.NewMessage(protocol.TypeRegister, protocol.RegisterPayload{
		ID:          c.config.Client.ID,
		Name:        c.config.Client.Name,
		Description: c.config.Client.Description,
		PID:         os.Getpid(),
		Path:        c.config.Path,
	})
	c.send <- registerMsg

	// 启动读写协程
	go c.readPump()
	go c.writePump()

	log.Printf("Connected to server: %s", url)
	return nil
}

// Disconnect 断开连接
func (c *Client) Disconnect() {
	// 使用原子操作确保只关闭一次
	if !atomic.CompareAndSwapInt32(&c.connClosed, 0, 1) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Run 运行客户端（带重连）
func (c *Client) Run() {
	// 初始化 tmux 管理器
	c.tmux = NewTmuxManager(c.config.Tmux)

	// 初始化 Claude 管理器
	c.config.Claude.ClientID = c.config.Client.ID
	c.claude = NewClaudeManager(c.config.Claude)

	// 从磁盘恢复会话事件
	c.loadSessionEvents()

	// 初始化权限系统
	if err := c.initPermissionSystem(); err != nil {
		log.Printf("Warning: failed to init permission system: %v", err)
	}

	if c.config.Tmux.AutoStart {
		if err := c.tmux.EnsureSession(); err != nil {
			log.Printf("Failed to create tmux session: %v", err)
		}
	}

	// 重连循环
	for {
		// 每次连接创建新的 context（旧的 cancel 后不会复用）
		c.ctx, c.cancel = context.WithCancel(context.Background())

		if err := c.Connect(); err != nil {
			log.Printf("Connection failed: %v", err)
			time.Sleep(time.Duration(c.config.Server.ReconnectInterval) * time.Second)
			continue
		}

		// 等待连接断开
		<-c.ctx.Done()
		c.Disconnect()

		log.Printf("Disconnected, reconnecting in %ds...", c.config.Server.ReconnectInterval)
		time.Sleep(time.Duration(c.config.Server.ReconnectInterval) * time.Second)
	}
}

// readPump 读取消息
func (c *Client) readPump() {
	defer func() {
		c.cancel()
	}()

	c.conn.SetReadLimit(512 * 1024)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			log.Printf("Read error: %v", err)
			break
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("JSON unmarshal error: %v", err)
			continue
		}

		c.handleMessage(&msg)
	}
}

// writePump 写入消息
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		// 只在连接未关闭时关闭（通过 Disconnect 的原子操作保证）
		c.mu.Lock()
		if c.conn != nil && atomic.LoadInt32(&c.connClosed) == 0 {
			c.conn.Close()
		}
		c.mu.Unlock()
	}()

	for {
		select {
		case <-c.ctx.Done():
			return

		case msg, ok := <-c.send:
			if !ok {
				return
			}

			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil || atomic.LoadInt32(&c.connClosed) == 1 {
				return
			}

			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("JSON marshal error: %v", err)
				continue
			}

			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil || atomic.LoadInt32(&c.connClosed) == 1 {
				return
			}

			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleMessage 处理消息
func (c *Client) handleMessage(msg *protocol.Message) {
	switch msg.Type {
	case protocol.TypeAck:
		log.Printf("Registered successfully")

	case protocol.TypeAttach:
		// 用户请求连接
		log.Printf("User attached: %s", msg.From)

		// 停止之前的 forwarding goroutine
		c.stopForwarding()

		// 确保 tmux 会话存在
		if err := c.tmux.EnsureSession(); err != nil {
			log.Printf("Failed to ensure tmux session: %v", err)
		}

		// 连接到 tmux 会话
		if err := c.tmux.Attach(); err != nil {
			log.Printf("Failed to attach to tmux: %v", err)
		}

		// 先发送当前屏幕内容给前端（在启动转发之前）
		if output, err := c.tmux.CaptureOutput(); err == nil && output != "" {
			outputMsg, _ := protocol.NewMessage(protocol.TypeOutput, protocol.OutputPayload{
				Data: output,
			})
			outputMsg.To = msg.From
			c.send <- outputMsg
		}

		// 启动转发
		go c.startForwarding(msg.From)

		// === 会话事件回放 ===
		// 1. 先复制当前所有事件快照
		c.sessionMu.Lock()
		events := make([]protocol.Message, len(c.sessionEvents))
		copy(events, c.sessionEvents)
		snapshotLen := len(c.sessionEvents)
		c.sentUpTo = snapshotLen
		c.sessionMu.Unlock()

		// 2. 回放快照事件（此时 setUser 还没调用，goroutine 不会发送）
		for i := range events {
			events[i].To = msg.From
			c.send <- &events[i]
		}

		// 3. 发送快照期间新增的事件（必须在 setUser 之前，避免竞态）
		c.sessionMu.Lock()
		for i := snapshotLen; i < len(c.sessionEvents); i++ {
			remaining := c.sessionEvents[i]
			remaining.To = msg.From
			c.send <- &remaining
		}
		c.sentUpTo = len(c.sessionEvents)
		c.sessionMu.Unlock()

		// 4. 设置用户 ID（在所有回放事件发送之后，此后 goroutine 可以发送新事件）
		c.setUser(msg.From)

		// 5. 如果 Claude 已完成，发送 chat_ready
		if !c.claude.IsRunning() {
			readyMsg, _ := protocol.NewMessage(protocol.TypeChatReady, protocol.SessionInfoPayload{
				SessionID: c.claude.SessionID(),
			})
			readyMsg.To = msg.From
			c.send <- readyMsg
			log.Printf("Sent chat_ready to reattached user (Claude idle, replayed %d events)", len(events))
		} else {
			log.Printf("Claude still running, replayed %d events, goroutine will send new ones", len(events))
		}

	case protocol.TypeDetach:
		log.Printf("User detached: %s", msg.From)
		c.setUser("")
		// 停止 forwarding
		c.stopForwarding()
		// 关闭 tmux 连接
		c.tmux.Close()

	case protocol.TypeInput:
		var payload protocol.InputPayload
		if err := msg.ParsePayload(&payload); err != nil {
			return
		}
		c.tmux.Write(payload.Data)

	case protocol.TypeResize:
		var payload protocol.ResizePayload
		if err := msg.ParsePayload(&payload); err != nil {
			return
		}
		c.tmux.Resize(payload.Cols, payload.Rows)

	case protocol.TypePong:
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	case protocol.TypeKillSession:
		log.Printf("Killing tmux session by user request")
		c.stopForwarding()
		c.tmux.Close()
		if err := c.tmux.KillSession(); err != nil {
			log.Printf("Failed to kill tmux session: %v", err)
		}

	case protocol.TypeChatInput:
		var payload protocol.ChatInputPayload
		if err := msg.ParsePayload(&payload); err != nil {
			return
		}
		// 将用户消息存入 sessionEvents，支持断线重连后完整回放
		userMsg, _ := protocol.NewMessage(protocol.TypeChatMessage, protocol.ChatMessagePayload{
			EventType: "user_message",
			Text:      payload.Text,
		})
		c.sessionMu.Lock()
		c.sessionEvents = append(c.sessionEvents, *userMsg)
		c.saveSessionEvents()
		c.sessionMu.Unlock()
		go c.handleChatInput(msg.From, payload.Text)

	case protocol.TypeNewSession:
		log.Printf("Starting new Claude session")
		c.claude.Abort()
		c.claude.ResetSession()
		c.sessionMu.Lock()
		c.sessionEvents = nil
		c.sentUpTo = 0
		c.saveSessionEvents()
		c.sessionMu.Unlock()

	case protocol.TypePermissionResponse:
		var payload protocol.PermissionResponsePayload
		if err := msg.ParsePayload(&payload); err != nil {
			log.Printf("[PERM] Failed to parse permission response: %v", err)
			return
		}
		log.Printf("[PERM] Client received permission_response: requestID=%s approved=%v", payload.RequestID, payload.Approved)
		if c.hookServer != nil {
			c.hookServer.HandleResponse(payload.RequestID, payload.Approved)
		} else {
			log.Printf("[PERM] hookServer is nil!")
		}

	case protocol.TypeSessionInfo:
		infoMsg, _ := protocol.NewMessage(protocol.TypeSessionInfo, protocol.SessionInfoPayload{
			SessionID: c.claude.SessionID(),
			Path:      c.config.Path,
		})
		infoMsg.To = msg.From
		c.send <- infoMsg
	}
}

// startForwarding 开始转发终端输出
func (c *Client) startForwarding(userID string) {
	// 创建独立的 context 用于此 forwarding 会话
	c.forwardMu.Lock()
	ctx, cancel := context.WithCancel(c.ctx)
	c.forwardCancel = cancel
	c.forwardMu.Unlock()

	currentUserID := func() string {
		c.userMu.RLock()
		defer c.userMu.RUnlock()
		return c.attachedUser
	}

	buf := make([]byte, 4096)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := c.tmux.Read(buf)
		if err != nil {
			log.Printf("Read from tmux error: %v", err)
			break
		}

		if n > 0 {
			msg, _ := protocol.NewMessage(protocol.TypeOutput, protocol.OutputPayload{
				Data: string(buf[:n]),
			})
			msg.To = currentUserID()
			if msg.To != "" {
				c.send <- msg
			}
		}
	}
}

// stopForwarding 停止 forwarding goroutine
func (c *Client) stopForwarding() {
	c.forwardMu.Lock()
	defer c.forwardMu.Unlock()

	if c.forwardCancel != nil {
		c.forwardCancel()
		c.forwardCancel = nil
	}
}

// handleChatInput 处理聊天输入，启动 Claude 并流式转发结果
func (c *Client) handleChatInput(userID string, text string) {
	// 动态获取当前连接的用户 ID（支持断线重连后切换）
	currentUserID := func() string {
		c.userMu.RLock()
		defer c.userMu.RUnlock()
		return c.attachedUser
	}

	// 立即发送确认，让 UI 知道消息已被接收
	ackMsg, _ := protocol.NewMessage(protocol.TypeChatAck, nil)
	ackMsg.To = currentUserID()
	c.send <- ackMsg

	if c.claude.IsRunning() {
		errMsg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
			Code:    409,
			Message: "Claude is still processing, please wait",
		})
		errMsg.To = currentUserID()
		c.send <- errMsg
		return
	}

	if err := c.claude.SendMessage(text); err != nil {
		errMsg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
			Code:    500,
			Message: fmt.Sprintf("Failed to start Claude: %v", err),
		})
		errMsg.To = currentUserID()
		c.send <- errMsg
		return
	}

	// 流式转发事件到用户
	// 事件同时追加到 sessionEvents（Client 级别），即使 goroutine 退出也不丢失
	for event := range c.claude.Stream() {
		msg, _ := protocol.NewMessage(protocol.TypeChatMessage, protocol.ChatMessagePayload{
			EventType:                string(event.Type),
			Text:                     event.Text,
			ToolID:                   event.ToolID,
			ToolName:                 event.ToolName,
			ToolInput:                event.ToolInput,
			ToolOutput:               event.ToolOutput,
			CostUSD:                  event.CostUSD,
			IsPartial:                event.IsPartial,
			SessionID:                event.SessionID,
			InputTokens:              event.InputTokens,
			OutputTokens:             event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens,
			CacheReadInputTokens:     event.CacheReadInputTokens,
			ContextWindow:            event.ContextWindow,
		})

		// 追加到会话事件缓冲，并判断是否应该发送给用户
		// 只有实际发送时才更新 sentUpTo，避免用户离线时标记为已发送导致回放跳过
		uid := currentUserID()
		c.sessionMu.Lock()
		c.sessionEvents = append(c.sessionEvents, *msg)
		myIndex := len(c.sessionEvents) - 1
		shouldSend := myIndex >= c.sentUpTo
		if shouldSend && uid != "" {
			c.sentUpTo = len(c.sessionEvents)
		}
		// 持久化事件到磁盘（result 事件时保存，避免频繁 IO）
		if event.Type == EventResult {
			c.saveSessionEvents()
		}
		c.sessionMu.Unlock()

		if uid != "" && shouldSend {
			msg.To = uid
			c.send <- msg
		}

		// result 事件表示轮次结束，发送 chat_ready
		if event.Type == EventResult {
			uid := currentUserID()
			if uid != "" {
				readyMsg, _ := protocol.NewMessage(protocol.TypeChatReady, protocol.SessionInfoPayload{
					SessionID: c.claude.SessionID(),
				})
				readyMsg.To = uid
				c.send <- readyMsg
			}
			// 如果用户不在线，chat_ready 由 TypeAttach handler 在重连时发送
		}
	}
}

// initPermissionSystem 初始化权限系统
func (c *Client) initPermissionSystem() error {
	checker, err := NewPermissionChecker()
	if err != nil {
		return fmt.Errorf("failed to init permission checker: %w", err)
	}

	timeout := 60 * time.Second

	// sendToUI 回调：将权限请求发送给当前连接的 Web UI 用户
	sendToUI := func(msg *protocol.Message) {
		c.userMu.RLock()
		uid := c.attachedUser
		c.userMu.RUnlock()
		if uid != "" {
			msg.To = uid
			c.send <- msg
		}
	}

	hs, err := NewHookServer(checker, timeout, sendToUI, c.config.Client.ID)
	if err != nil {
		return fmt.Errorf("failed to start hook server: %w", err)
	}
	c.hookServer = hs
	log.Printf("Permission hook server started on port %d", hs.Port())

	c.claude.SetHookSettingsPath(hs.SettingsPath())
	log.Printf("Hook settings file: %s", hs.SettingsPath())

	return nil
}

// setUser 设置当前连接的用户 ID
func (c *Client) setUser(userID string) {
	c.userMu.Lock()
	defer c.userMu.Unlock()
	c.attachedUser = userID
}

// sessionEventsPath 返回 sessionEvents 持久化文件路径
// 每个 client 使用独立文件，避免多客户端共享导致会话记录混淆
func (c *Client) sessionEventsPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	clientID := c.config.Client.ID
	if clientID == "" {
		clientID = "default"
	}
	return filepath.Join(dir, ".claude-forward", fmt.Sprintf("session_events_%s.json", clientID))
}

// saveSessionEvents 将 sessionEvents 持久化到磁盘（调用方需持有 c.sessionMu）
func (c *Client) saveSessionEvents() {
	path := c.sessionEventsPath()
	if path == "" {
		return
	}
	data, err := json.Marshal(c.sessionEvents)
	if err != nil {
		log.Printf("Failed to marshal session events: %v", err)
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("Failed to save session events: %v", err)
	}
}

// loadSessionEvents 从磁盘恢复 sessionEvents
func (c *Client) loadSessionEvents() {
	path := c.sessionEventsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var events []protocol.Message
	if err := json.Unmarshal(data, &events); err != nil {
		log.Printf("Failed to unmarshal session events: %v", err)
		return
	}
	if len(events) > 0 {
		c.sessionEvents = events
		log.Printf("Restored %d session events from file", len(events))
	}
}
