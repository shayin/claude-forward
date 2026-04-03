package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	forwardCancel context.CancelFunc // 用于停止 forwarding goroutine
	forwardMu     sync.Mutex
	connClosed    int32 // 连接是否已关闭的标志（用于防止重复关闭）
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
	c.claude = NewClaudeManager(c.config.Claude)

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
		// 这样可以确保刷新页面后能立即显示内容
		if output, err := c.tmux.CaptureOutput(); err == nil && output != "" {
			outputMsg, _ := protocol.NewMessage(protocol.TypeOutput, protocol.OutputPayload{
				Data: output,
			})
			outputMsg.To = msg.From
			c.send <- outputMsg
			log.Printf("Sent captured screen content: %d bytes", len(output))
		}

		// 启动转发
		go c.startForwarding(msg.From)

	case protocol.TypeDetach:
		log.Printf("User detached: %s", msg.From)
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
		go c.handleChatInput(msg.From, payload.Text)

	case protocol.TypeNewSession:
		log.Printf("Starting new Claude session")
		c.claude.Abort()
		c.claude.ResetSession()

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
			msg.To = userID
			c.send <- msg
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
	// 立即发送确认，让 UI 知道消息已被接收
	ackMsg, _ := protocol.NewMessage(protocol.TypeChatAck, nil)
	ackMsg.To = userID
	c.send <- ackMsg

	if c.claude.IsRunning() {
		errMsg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
			Code:    409,
			Message: "Claude is still processing, please wait",
		})
		errMsg.To = userID
		c.send <- errMsg
		return
	}

	if err := c.claude.SendMessage(text); err != nil {
		errMsg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
			Code:    500,
			Message: fmt.Sprintf("Failed to start Claude: %v", err),
		})
		errMsg.To = userID
		c.send <- errMsg
		return
	}

	// 流式转发事件到用户
	for event := range c.claude.Stream() {
		msg, _ := protocol.NewMessage(protocol.TypeChatMessage, protocol.ChatMessagePayload{
			EventType:  string(event.Type),
			Text:       event.Text,
			ToolID:     event.ToolID,
			ToolName:   event.ToolName,
			ToolInput:  event.ToolInput,
			ToolOutput: event.ToolOutput,
			CostUSD:    event.CostUSD,
			IsPartial:  event.IsPartial,
			SessionID:  event.SessionID,
		})
		msg.To = userID
		c.send <- msg

		// result 事件表示轮次结束
		if event.Type == EventResult {
			readyMsg, _ := protocol.NewMessage(protocol.TypeChatReady, protocol.SessionInfoPayload{
				SessionID: c.claude.SessionID(),
			})
			readyMsg.To = userID
			c.send <- readyMsg
		}
	}
}
