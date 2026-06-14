package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shayin/claude-forward/internal/protocol"
)

// Client 客户端
type Client struct {
	config        *Config
	encryptionKey []byte // 应用层加密密钥
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
	connGen       int64 // 连接代数，每次 Connect 递增（用于检测断线重连）

	// 会话级别事件缓冲：存储当前 Claude 会话的所有事件
	// 在 handleChatInput goroutine 退出后仍保留，支持断线重连后完整回放
	sessionEvents []protocol.Message
	sentUpTo      int         // 已发送到用户的事件索引（用于去重）
	sessionMu     sync.Mutex  // 保护 sessionEvents 和 sentUpTo

	// 后台模式：超时后任务转为后台继续运行
	// 受 bgMu 保护：handleMessage 写入，handleChatInput 读取/清零
	bgMu       sync.Mutex
	bgMode         bool   // 当前是否处于后台模式
	bgTaskID       string // 后台任务 ID
	bgWechatID     string // 完成后推送给谁
	currentWechatID string // 当前正在处理的微信用户 ID（来自 ChatInputPayload.WechatID）
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

	// 递增连接代数
	atomic.AddInt64(&c.connGen, 1)

	// 初始化加密密钥
	if c.config.Server.EncryptionKey != "" {
		c.encryptionKey = protocol.DeriveKey(c.config.Server.EncryptionKey)
	}

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
		ClawbotID:   c.config.Client.ClawbotID,
		PID:         os.Getpid(),
		Path:        c.config.Path,
	})
	c.send <- registerMsg

	// 启动读写协程
	go c.readPump()
	go c.writePump()

	log.Printf("Connected to server: %s", url)

	// 重连后重发未送达的后台任务结果
	c.resendPendingResults()

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

// Shutdown 优雅关闭：停止 Claude 子进程、停止 Run 协程、断开连接
func (c *Client) Shutdown() {
	// 1. 停止 Claude 子进程
	if c.claude != nil {
		c.claude.Abort()
	}
	// 2. 取消客户端上下文（停止 Run 重连循环）
	c.cancel()
	// 3. 断开 WebSocket
	c.Disconnect()
	// 4. 关闭 tmux
	if c.tmux != nil {
		c.tmux.Close()
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

	// 重连循环（指数退避）
	var retryCount int
	const maxRetryDelay = 60 // 最大退避秒数
	baseInterval := time.Duration(c.config.Server.ReconnectInterval) * time.Second

	for {
		// 每次连接创建新的 context（旧的 cancel 后不会复用）
		c.ctx, c.cancel = context.WithCancel(context.Background())

		if err := c.Connect(); err != nil {
			retryCount++
			delay := time.Duration(retryCount*retryCount*2) * time.Second
			if delay.Seconds() > float64(maxRetryDelay) {
				delay = time.Duration(maxRetryDelay) * time.Second
			}
			log.Printf("Connection failed (attempt %d), retrying in %ds: %v", retryCount, int(delay.Seconds()), err)
			time.Sleep(delay)
			continue
		}

		// 连接成功，重置计数
		retryCount = 0

		// 等待连接断开
		<-c.ctx.Done()
		c.Disconnect()

		log.Printf("Disconnected, reconnecting in %ds...", int(baseInterval.Seconds()))
		time.Sleep(baseInterval)
	}
}

// readPump 读取消息
func (c *Client) readPump() {
	defer func() {
		c.cancel()
	}()

	c.conn.SetReadLimit(512 * 1024)
	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
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

		// 应用层解密
		decrypted, err := protocol.DecryptMessage(c.encryptionKey, &msg)
		if err != nil {
			log.Printf("Decrypt error: %v", err)
			continue
		}

		c.handleMessage(decrypted)
	}
}

// writePump 写入消息
func (c *Client) writePump() {
	ticker := time.NewTicker(15 * time.Second)
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

			// 应用层加密
			encrypted, err := protocol.EncryptMessage(c.encryptionKey, msg)
			if err != nil {
				log.Printf("Encrypt error (fatal): %v", err)
				return
			}

			data, err := json.Marshal(encrypted)
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

		if strings.HasPrefix(msg.From, "bot-") {
			// Bot API 连接不需要回放历史事件，直接设置用户 ID
			c.setUser(msg.From)
		} else {
			// Web UI 连接：回放全部 sessionEvents（支持断线重连恢复完整对话）
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
		// 检查是否为客户端命令（如 /provider）
		if strings.HasPrefix(payload.Text, "/provider") {
			go c.handleProviderCommand(msg.From, payload.Text)
			return
		}
		// 记录当前微信用户 ID（来自 Server 下发），供断线自动后台时使用
		if payload.WechatID != "" {
			c.bgMu.Lock()
			c.currentWechatID = payload.WechatID
			c.bgMu.Unlock()
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
		if strings.HasPrefix(msg.From, "bot-") {
			// Bot API：仅重置 Bot 自己的 session
			log.Printf("Starting new Bot session")
			c.claude.SetBotSessionID("")
		} else {
			// Web UI：重置主会话和事件
			log.Printf("Starting new Claude session")
			c.claude.Abort()
			c.claude.ResetSession()
			c.sessionMu.Lock()
			c.sessionEvents = nil
			c.sentUpTo = 0
			c.saveSessionEvents()
			c.sessionMu.Unlock()
		}

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

	case protocol.TypeBackgroundMode:
		var payload protocol.BackgroundModePayload
		if err := msg.ParsePayload(&payload); err != nil {
			log.Printf("[BG] Failed to parse background_mode: %v", err)
			return
		}
		log.Printf("[BG] Received background_mode: taskID=%s wechatID=%s", payload.TaskID, payload.WechatID)
		c.bgMu.Lock()
		c.bgMode = true
		c.bgTaskID = payload.TaskID
		c.bgWechatID = payload.WechatID
		c.bgMu.Unlock()

	case protocol.TypeConfigUpdate:
		var payload protocol.ConfigUpdatePayload
		if err := msg.ParsePayload(&payload); err != nil {
			log.Printf("Failed to parse config_update: %v", err)
			return
		}
		c.handleConfigUpdate(msg.From, payload.EnvFile)

	case protocol.TypeConfigInfo:
		info := c.getConfigInfo()
		infoMsg, _ := protocol.NewMessage(protocol.TypeConfigInfo, info)
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

	// Bot API 使用独立 session（与 Web UI 隔离），不存 sessionEvents
	isBot := strings.HasPrefix(userID, "bot-")
	var resumeSessionID string
	if isBot {
		resumeSessionID = c.claude.BotSessionID()
	} else {
		resumeSessionID = c.claude.SessionID()
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

	if err := c.claude.SendMessage(text, resumeSessionID); err != nil {
		errMsg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
			Code:    500,
			Message: fmt.Sprintf("Failed to start Claude: %v", err),
		})
		errMsg.To = currentUserID()
		c.send <- errMsg
		return
	}

	// 流式转发事件到用户
	// åå°æ¨¡å¼ï¼æ¶éå®æ´ç»æç¨äºå¼æ­¥æ¨é
	// 新任务启动，清除上次后台模式的残留状态。
	// 否则上一次 bgMode=true 会让新任务的事件走 bg 路径（收集到 bgFullText），
	// Server 端 chatViaHub 收不到任何事件，3 分钟后必然超时，
	// 又发 background_mode 把 bgMode 设回 true，形成"每句话都误报超时"的死循环。
	if isBot {
		c.bgMu.Lock()
		c.bgMode = false
		c.bgTaskID = ""
		c.bgWechatID = ""
		c.bgMu.Unlock()
	}

	var bgFullText string
	var bgHasStreamDelta bool
	var bgCostUSD float64
	var bgIsError bool
	var bgErrorMsg string
	var bgEventCount int
	var bgLastLog time.Time
	var bgActive bool

	// 追踪是否收到 result/error 事件，用于检测非正常结束
	var gotResult bool
	var gotError bool
	var lastErrorMsg string

	// 记录开始时的连接代数，用于检测断线重连
	startGen := atomic.LoadInt64(&c.connGen)

	// 无事件超时：如果超过 30 分钟没有任何事件，认为 Claude 进程卡死
	const noOutputTimeout = 30 * time.Minute
	noOutputTimer := time.NewTimer(noOutputTimeout)
	defer noOutputTimer.Stop()

	// 用 select 包装事件循环，支持超时终止
	streamCh := c.claude.Stream()
	for {
		select {
		case event, ok := <-streamCh:
			if !ok {
				goto streamEnded
			}
			noOutputTimer.Reset(noOutputTimeout)
		// 从事件中捕获 session_id，分别持久化
		if event.SessionID != "" {
			if isBot {
				c.claude.SetBotSessionID(event.SessionID)
			} else {
				c.claude.SetSessionID(event.SessionID)
			}
		}

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

		uid := currentUserID()

		if isBot {
			// Bot API：直接发送，不存 sessionEvents
			// 后台模式下跳过发送（无人接收，避免 c.send 阻塞）
			if uid != "" && !bgActive {
				msg.To = uid
				c.send <- msg
			}
			// 后台模式：收集文本用于异步推送
			switch event.Type {
			case EventStreamDelta:
				bgHasStreamDelta = true
				bgFullText += event.Text
			case EventText:
				if !bgHasStreamDelta {
					bgFullText = event.Text
				}
			case EventResult:
				if !bgHasStreamDelta && bgFullText == "" {
					bgFullText = event.Text
				}
				bgCostUSD = event.CostUSD
				gotResult = true
			case EventError:
				bgIsError = true
				bgErrorMsg = event.Text
				gotError = true
				lastErrorMsg = event.Text
			}
			bgEventCount++

			// 检测连接代数变化（服务器重启/断线重连），自动切换后台模式
			c.bgMu.Lock()
			bgActive = c.bgMode
			if !bgActive && atomic.LoadInt64(&c.connGen) != startGen {
				c.bgMode = true
				c.bgTaskID = fmt.Sprintf("auto-bg-%d", time.Now().UnixMilli())
				c.bgWechatID = c.currentWechatID
				bgActive = true
				log.Printf("[BG] Connection lost mid-task, auto-switched to background mode: taskID=%s wechatID=%s", c.bgTaskID, c.bgWechatID)
			}
			c.bgMu.Unlock()

			if bgActive && time.Since(bgLastLog) >= 30*time.Second {
				bgLastLog = time.Now()
				log.Printf("[BG] Task still running: events=%d textLen=%d", bgEventCount, len(bgFullText))
			}
		} else {
			// Web UI：追加到会话事件缓冲，支持断线重连回放
			c.sessionMu.Lock()
			c.sessionEvents = append(c.sessionEvents, *msg)
			myIndex := len(c.sessionEvents) - 1
			shouldSend := myIndex >= c.sentUpTo
			if shouldSend && uid != "" {
				c.sentUpTo = len(c.sessionEvents)
			}
			if event.Type == EventResult {
				c.saveSessionEvents()
			}
			c.sessionMu.Unlock()

			if uid != "" && shouldSend {
				msg.To = uid
				c.send <- msg
			}
		}

		// result 事件表示轮次结束，发送 chat_ready
		if event.Type == EventResult && !bgActive {
			uid := currentUserID()
			if uid != "" {
				readyMsg, _ := protocol.NewMessage(protocol.TypeChatReady, protocol.SessionInfoPayload{
					SessionID: c.claude.SessionID(),
				})
				readyMsg.To = uid
				c.send <- readyMsg
			}
		}
		case <-noOutputTimer.C:
			// 30 min no events, Claude stuck, force kill
			log.Printf("[TIMEOUT] No events for %v, killing Claude process", noOutputTimeout)
			c.claude.Abort()
			gotError = true
			lastErrorMsg = fmt.Sprintf("任务超时：超过 %v 无响应，已自动终止", noOutputTimeout)
		}
	}

streamEnded:
	log.Printf("[BG] Stream ended, checking background mode: bgMode=%v textLen=%d", c.bgMode, len(bgFullText))

	// 非正常结束：收到 error 但没有收到 result（如 context window 满）
	if !gotResult && gotError {
		log.Printf("[CTX] Stream ended with error, no result: isBot=%v msg=%s", isBot, lastErrorMsg)
		if isBot {
			// Bot（微信端）：主动推送错误通知
			if !bgActive {
				c.bgMu.Lock()
				wechatID := c.currentWechatID
				c.bgMu.Unlock()
				if wechatID != "" {
					pushText := lastErrorMsg
					if isContextWindowError(lastErrorMsg) {
						pushText = "上下文已满，已自动新建会话，请重新发送你的消息。"
					}
					resultMsg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, protocol.BackgroundResultPayload{
						TaskID:   fmt.Sprintf("error-%d", time.Now().UnixMilli()),
						WechatID: wechatID,
						IsError:  true,
						ErrorMsg: pushText,
					})
					select {
					case c.send <- resultMsg:
						log.Printf("[CTX] Error pushed to wechat")
					case <-time.After(10 * time.Second):
						log.Printf("[CTX] WARNING: Failed to push error to wechat")
					}
				}
			}
			// 自动重置 bot session
			if isContextWindowError(lastErrorMsg) {
				log.Printf("[CTX] Context window full, auto-resetting bot session")
				c.claude.SetBotSessionID("")
			}
		} else {
			// Web UI：发送 chat_error + chat_ready 恢复前端交互
			uid := currentUserID()
			if uid != "" {
				errMsg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
					Code:    400,
					Message: lastErrorMsg,
				})
				errMsg.To = uid
				c.send <- errMsg

				readyMsg, _ := protocol.NewMessage(protocol.TypeChatReady, protocol.SessionInfoPayload{
					SessionID: c.claude.SessionID(),
				})
				readyMsg.To = uid
				c.send <- readyMsg
			}
			// 自动重置 Web UI session
			if isContextWindowError(lastErrorMsg) {
				log.Printf("[CTX] Context window full, auto-resetting session")
				c.claude.ResetSession()
				c.sessionMu.Lock()
				c.sessionEvents = nil
				c.sentUpTo = 0
				c.saveSessionEvents()
				c.sessionMu.Unlock()
			}
		}
	}

	// 后台模式：任务完成，发送结果给 Server 推送
	// Bot 模式下等待确保 TypeBackgroundMode 消息被 handleMessage 处理
	// 避免竞态：Claude 在超时消息到达 Client 之前完成，导致 bgMode 未设置
	if isBot {
		for i := 0; i < 50; i++ {
			c.bgMu.Lock()
			if c.bgMode {
				c.bgMu.Unlock()
				break
			}
			c.bgMu.Unlock()
			time.Sleep(200 * time.Millisecond)
		}
	}
	c.bgMu.Lock()
	bgActive = c.bgMode && c.bgWechatID != ""
	bgTask := c.bgTaskID
	bgWechat := c.bgWechatID
	if bgActive {
		c.bgMode = false
		c.bgTaskID = ""
		c.bgWechatID = ""
	}
	c.bgMu.Unlock()

	if bgActive {
		log.Printf("[BG] Background task completed: taskID=%s textLen=%d", bgTask, len(bgFullText))
		resultMsg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, protocol.BackgroundResultPayload{
			TaskID:   bgTask,
			WechatID: bgWechat,
			FullText: bgFullText,
			IsError:  bgIsError,
			ErrorMsg: bgErrorMsg,
			CostUSD:  bgCostUSD,
		})
		select {
		case c.send <- resultMsg:
			log.Printf("[BG] BackgroundResult sent successfully")
		case <-time.After(30 * time.Second):
			log.Printf("[BG] WARNING: Failed to send BackgroundResult, persisting to disk")
			c.savePendingResult(protocol.BackgroundResultPayload{
				TaskID:   bgTask,
				WechatID: bgWechat,
				FullText: bgFullText,
				IsError:  bgIsError,
				ErrorMsg: bgErrorMsg,
				CostUSD:  bgCostUSD,
			})
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

	hs, err := NewHookServer(checker, timeout, sendToUI, c.config.Client.ID, c.config.Claude.EnvFile)
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

// isContextWindowError 检测是否为 context window 满相关的错误
func isContextWindowError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "context window") ||
		strings.Contains(lower, "token limit") ||
		strings.Contains(lower, "prompt is too long") ||
		strings.Contains(lower, "input is too long")
}

// pendingResultsPath 返回待重发的后台结果文件路径
func (c *Client) pendingResultsPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	clientID := c.config.Client.ID
	if clientID == "" {
		clientID = "default"
	}
	return filepath.Join(dir, ".claude-forward", "pending_results", clientID+".json")
}

// savePendingResult 持久化未发送的后台结果到磁盘
func (c *Client) savePendingResult(payload protocol.BackgroundResultPayload) {
	path := c.pendingResultsPath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[BG] Failed to marshal pending result: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[BG] Failed to save pending result: %v", err)
	} else {
		log.Printf("[BG] Pending result saved to %s", path)
	}
}

// resendPendingResults 重发未发送的后台结果
func (c *Client) resendPendingResults() {
	path := c.pendingResultsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // 没有残留文件
	}
	var payload protocol.BackgroundResultPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[BG] Failed to parse pending result: %v", err)
		os.Remove(path)
		return
	}
	log.Printf("[BG] Resending pending result: taskID=%s wechatID=%s", payload.TaskID, payload.WechatID)
	resultMsg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
	select {
	case c.send <- resultMsg:
		os.Remove(path)
		log.Printf("[BG] Pending result resent successfully")
	case <-time.After(10 * time.Second):
		log.Printf("[BG] WARNING: Failed to resend pending result, will retry next reconnect")
	}
}

// handleProviderCommand 处理 /provider 聊天命令
func (c *Client) handleProviderCommand(userID string, text string) {
	parts := strings.Fields(text) // "/provider", subcommand/name
	subcmd := ""
	if len(parts) >= 2 {
		subcmd = parts[1]
	}

	var reply string

	switch subcmd {
	case "list":
		providers := c.listProviders()
		if len(providers) == 0 {
			reply = "没有找到可用的 provider（请检查 provider_dir 配置）"
		} else {
			current := c.claude.GetEnvFile()
			reply = "可用 providers:\n"
			for _, p := range providers {
				marker := "  "
				if current != "" && strings.Contains(current, p+".sh") {
					marker = "* "
				}
				reply += fmt.Sprintf("  %s%s\n", marker, p)
			}
			reply += "\n使用 /provider <name> 切换"
		}

	case "status":
		envFile := c.claude.GetEnvFile()
		if envFile == "" {
			reply = "当前未配置 env_file，使用 settings.json 的默认 env"
		} else {
			reply = fmt.Sprintf("当前 provider: %s", envFile)
		}

	case "":
		reply = "用法:\n  /provider list    - 列出可用 providers\n  /provider <name>  - 切换到指定 provider\n  /provider status  - 查看当前 provider"

	default:
		// 切换 provider: /provider deepseek
		if err := c.switchProvider(subcmd); err != nil {
			c.sendChatError(userID, err.Error())
			return
		}
		envFile := c.claude.GetEnvFile()
		reply = fmt.Sprintf("已切换到 %s", filepath.Base(envFile))
	}

	// 发送回复（用 "text" 事件类型，让 server 端 wechat 能识别）
	c.sendChatText(userID, reply)
	c.sendChatReady(userID)
}

// handleConfigUpdate 处理 TypeConfigUpdate 协议消息（Web UI）
func (c *Client) handleConfigUpdate(userID string, envFile string) {
	if envFile == "" {
		info := c.getConfigInfo()
		info.Error = "env_file is required"
		infoMsg, _ := protocol.NewMessage(protocol.TypeConfigInfo, info)
		infoMsg.To = userID
		c.send <- infoMsg
		return
	}
	if err := c.switchProvider(envFile); err != nil {
		info := c.getConfigInfo()
		info.Error = err.Error()
		infoMsg, _ := protocol.NewMessage(protocol.TypeConfigInfo, info)
		infoMsg.To = userID
		c.send <- infoMsg
		return
	}
	info := c.getConfigInfo()
	infoMsg, _ := protocol.NewMessage(protocol.TypeConfigInfo, info)
	infoMsg.To = userID
	c.send <- infoMsg
}

// switchProvider 切换到指定 provider（接受名称或完整路径）
func (c *Client) switchProvider(nameOrPath string) error {
	var envFilePath string

	// 如果是完整路径（含 / 或 .sh），直接使用
	if strings.Contains(nameOrPath, "/") || strings.HasSuffix(nameOrPath, ".sh") {
		envFilePath = nameOrPath
	} else {
		// 是 provider 名称，拼接路径
		providerDir := c.config.Claude.ProviderDir
		if providerDir == "" {
			providerDir = filepath.Join(os.Getenv("HOME"), ".claude", "providers")
		}
		// 展开 ~ 前缀
		if strings.HasPrefix(providerDir, "~/") {
			providerDir = filepath.Join(os.Getenv("HOME"), providerDir[2:])
		}
		envFilePath = filepath.Join(providerDir, nameOrPath+".sh")
	}

	// 验证文件存在
	if _, err := os.Stat(envFilePath); os.IsNotExist(err) {
		return fmt.Errorf("Provider file not found: %s", envFilePath)
	}

	// 更新 ClaudeManager 的 env_file
	c.claude.UpdateEnvFile(envFilePath)

	// 重新生成 hooks-settings.json
	if c.hookServer != nil {
		if err := c.hookServer.UpdateEnvFile(envFilePath); err != nil {
			log.Printf("Failed to regenerate settings: %v", err)
		}
	}

	log.Printf("Provider switched to: %s", envFilePath)
	return nil
}

// sendChatText 发送聊天文本消息给用户
func (c *Client) sendChatText(userID string, text string) {
	msg, _ := protocol.NewMessage(protocol.TypeChatMessage, protocol.ChatMessagePayload{
		EventType: "text",
		Text:      text,
	})
	msg.To = userID
	c.send <- msg
}

// sendChatReady 发送 chat_ready 给用户
func (c *Client) sendChatReady(userID string) {
	msg, _ := protocol.NewMessage(protocol.TypeChatReady, nil)
	msg.To = userID
	c.send <- msg
}

// sendChatError 发送 chat_error 给用户
func (c *Client) sendChatError(userID string, errMsg string) {
	msg, _ := protocol.NewMessage(protocol.TypeChatError, protocol.ErrorPayload{
		Code:    500,
		Message: errMsg,
	})
	msg.To = userID
	c.send <- msg
}

// getConfigInfo 获取当前配置信息
func (c *Client) getConfigInfo() protocol.ConfigInfoPayload {
	return protocol.ConfigInfoPayload{
		EnvFile:   c.claude.GetEnvFile(),
		Providers: c.listProviders(),
	}
}

// listProviders 扫描 provider_dir 下的可用 provider 列表
func (c *Client) listProviders() []string {
	providerDir := c.config.Claude.ProviderDir
	if providerDir == "" {
		providerDir = filepath.Join(os.Getenv("HOME"), ".claude", "providers")
	}
	if strings.HasPrefix(providerDir, "~/") {
		providerDir = filepath.Join(os.Getenv("HOME"), providerDir[2:])
	}

	entries, err := os.ReadDir(providerDir)
	if err != nil {
		return nil
	}

	var providers []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sh") && name != "init.sh" {
			providers = append(providers, strings.TrimSuffix(name, ".sh"))
		}
	}
	return providers
}
