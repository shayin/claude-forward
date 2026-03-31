package client

import (
	"context"
	"encoding/json"
	"log"
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

	if c.config.Tmux.AutoStart {
		if err := c.tmux.EnsureSession(); err != nil {
			log.Printf("Failed to create tmux session: %v", err)
		}
	}

	// 重连循环
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		if err := c.Connect(); err != nil {
			log.Printf("Connection failed: %v", err)
			time.Sleep(time.Duration(c.config.Server.ReconnectInterval) * time.Second)
			continue
		}

		// 等待连接断开
		<-c.ctx.Done()
		c.Disconnect()

		// 等待重连
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
