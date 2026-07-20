package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
)

// wechatUserState 微信用户的完整状态
type wechatUserState struct {
	Route       UserRoute
	Bot         *ILinkBot
	LoginResult *ILinkLoginResult
	// 微信会话绑定：发送消息的 wechatUserID → clientID
	// 注意：一个 iLink bot 可能收到多个微信用户的消息（如果 bot 允许）
	// 但在当前设计中，每个 bot 只服务于一个配置用户
	Bindings      map[string]string // wechatUserID → clientID
	TypingTickets map[string]string // wechatUserID → typingTicket

	// 登录流程状态
	QRPending  bool
	QRToken    string
	QRCodeURL  string
	QRDeadline time.Time

	// Push 离线队列
	PushQueue      []pushQueueItem
	pushMu         sync.Mutex // 串行化同一用户的队列投递，避免定时重试与登录重放并发
	pushRetryAt    time.Time
	pushRetryDelay time.Duration

	// P1-a: 消息串行队列（保证同一用户消息按发送序处理，避免并发抢写导致顺序错乱/丢失）
	MsgQueue chan ILinkIncomingMessage

	// 控制
	stopCh  chan struct{}
	stopped bool
}

// WeChatManager 微信多用户管理器
type WeChatManager struct {
	hub     *Hub
	auth    *Auth
	config  WeChatConfig
	users   map[string]*wechatUserState // index (配置序号) → state
	dataDir string
	mu      sync.Mutex
}

// NewWeChatManager 创建管理器
func NewWeChatManager(hub *Hub, auth *Auth, cfg WeChatConfig) *WeChatManager {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "wechat-data"
	}

	users := make(map[string]*wechatUserState)
	for i, route := range cfg.Users {
		users[fmt.Sprintf("%d", i)] = &wechatUserState{
			Route:         route,
			Bot:           NewILinkBot(),
			Bindings:      make(map[string]string),
			TypingTickets: make(map[string]string),
			MsgQueue:      make(chan ILinkIncomingMessage, 64),
			stopCh:        make(chan struct{}),
		}
	}

	return &WeChatManager{
		hub:     hub,
		auth:    auth,
		config:  cfg,
		users:   users,
		dataDir: dataDir,
	}
}

// Start 启动所有微信用户会话
func (m *WeChatManager) Start() {
	// P1-a: 为每个用户启动串行消息处理 goroutine（永久运行，stopCh 退出）
	for idx, user := range m.users {
		go m.processLoop(idx, user)
		go m.pushRetryLoop(idx, user)
	}
	// P2: 恢复 UpdateBuf（避免服务端重启后游标归零导致 iLink 重投递断线期间消息）
	for idx, user := range m.users {
		m.loadBuf(idx, user)
	}
	for idx, user := range m.users {
		// 加载离线队列
		m.loadPushQueue(idx, user)

		// 尝试加载已保存的 session
		session := m.loadSession(idx)
		if session != nil {
			user.Bot.Token = session.BotToken
			user.Bot.BaseURL = session.BaseURL
			if user.Bot.ValidateSession() {
				user.LoginResult = session
				log.Printf("[WECHAT] 用户 %s (%s) 恢复登录成功", idx, user.Route.WechatID)
				// 投递离线队列
				m.flushPushQueue(idx, user)
				go m.pollLoop(idx, user)
				continue
			}
			log.Printf("[WECHAT] 用户 %s (%s) session 已失效", idx, user.Route.WechatID)
			user.Bot.Token = ""
			user.Bot.BaseURL = ilinkDefaultBaseURL
		}

		// 加载 bindings
		m.loadBindings(idx, user)

		// 未登录的标记为待扫码
		log.Printf("[WECHAT] 用户 %s (%s) 等待扫码登录", idx, user.Route.WechatID)
	}

	// 加载 bindings
	for idx, user := range m.users {
		m.loadBindings(idx, user)
	}
}

const (
	pushRetryInterval = 5 * time.Second
	pushRetryInitial  = 10 * time.Second
	pushRetryMax      = 5 * time.Minute
)

// pushRetryLoop 让在线状态下暂时失败的 PushQueue 自动重试；失败条目仍保留在磁盘。
func (m *WeChatManager) pushRetryLoop(idx string, user *wechatUserState) {
	ticker := time.NewTicker(pushRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			m.mu.Lock()
			loggedIn := user.LoginResult != nil && !user.stopped
			queued := len(user.PushQueue) > 0
			due := user.pushRetryAt.IsZero() || !now.Before(user.pushRetryAt)
			m.mu.Unlock()
			if loggedIn && queued && due {
				if m.flushPushQueue(idx, user) {
					m.resetPushRetry(user)
				} else {
					m.schedulePushRetry(user, now)
				}
			}
		case <-user.stopCh:
			return
		}
	}
}

func (m *WeChatManager) resetPushRetry(user *wechatUserState) {
	m.mu.Lock()
	user.pushRetryAt, user.pushRetryDelay = time.Time{}, 0
	m.mu.Unlock()
}

func (m *WeChatManager) schedulePushRetry(user *wechatUserState, now time.Time) {
	m.mu.Lock()
	if user.pushRetryDelay == 0 {
		user.pushRetryDelay = pushRetryInitial
	} else {
		user.pushRetryDelay *= 2
		if user.pushRetryDelay > pushRetryMax {
			user.pushRetryDelay = pushRetryMax
		}
	}
	user.pushRetryAt = now.Add(user.pushRetryDelay)
	m.mu.Unlock()
}

// Stop 停止所有会话
func (m *WeChatManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for idx, user := range m.users {
		if !user.stopped && user.LoginResult != nil {
			close(user.stopCh)
			user.stopped = true
			m.saveBuf(idx, user) // P2: 落盘 UpdateBuf，重启后游标不归零
			log.Printf("[WECHAT] 停止用户 %s (%s)", idx, user.Route.WechatID)
		}
	}
}

// StartQRLogin 为指定用户启动 QR 码登录流程
func (m *WeChatManager) StartQRLogin(idx string) (*ILinkQRStartResult, error) {
	m.mu.Lock()
	user, ok := m.users[idx]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("用户 %s 不存在", idx)
	}

	// 如果已在轮询，先停止
	if !user.stopped && user.LoginResult != nil {
		close(user.stopCh)
		user.stopped = true
		user.stopCh = make(chan struct{})
	}

	// 创建新的 bot 实例
	user.Bot = NewILinkBot()
	user.LoginResult = nil
	m.mu.Unlock()

	qrResult, err := user.Bot.FetchQRCode()
	if err != nil {
		return nil, fmt.Errorf("获取二维码失败: %w", err)
	}

	m.mu.Lock()
	user.QRPending = true
	user.QRToken = qrResult.Token
	user.QRCodeURL = qrResult.QRCodeURL
	user.QRDeadline = time.Now().Add(8 * time.Minute)
	m.mu.Unlock()

	// 后台等待扫码
	go func() {
		result, err := user.Bot.WaitForLogin(qrResult.Token, 8*time.Minute)
		m.mu.Lock()
		user.QRPending = false
		if err != nil {
			log.Printf("[WECHAT] 用户 %s 扫码登录失败: %v", idx, err)
			m.mu.Unlock()
			return
		}

		user.LoginResult = result
		user.Bot.Token = result.BotToken
		user.Bot.BaseURL = result.BaseURL
		m.mu.Unlock()

		m.saveSession(idx, result)
		log.Printf("[WECHAT] 用户 %s (%s) 登录成功 BotID=%s", idx, user.Route.WechatID, result.BotID)

		// 投递离线队列
		m.flushPushQueue(idx, user)

		// 启动消息轮询
		go m.pollLoop(idx, user)
	}()

	return qrResult, nil
}

// GetStatus 获取所有用户状态
func (m *WeChatManager) GetStatus() []WeChatUserStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	var statuses []WeChatUserStatus
	for idx, user := range m.users {
		status := WeChatUserStatus{
			Index:     idx,
			WechatID:  user.Route.WechatID,
			ClawbotID: user.Route.ClawbotID,
		}

		if user.LoginResult != nil {
			status.LoggedIn = true
			status.BotID = user.LoginResult.BotID
		} else if user.QRPending {
			status.Pending = true
			status.QRCodeURL = user.QRCodeURL
		}

		statuses = append(statuses, status)
	}
	return statuses
}

// WeChatUserStatus 微信用户状态（API 返回）
type WeChatUserStatus struct {
	Index     string `json:"index"`
	WechatID  string `json:"wechat_id"`
	ClawbotID string `json:"clawbot_id"`
	LoggedIn  bool   `json:"logged_in"`
	Pending   bool   `json:"pending"`
	QRCodeURL string `json:"qr_code_url,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
}

// pollLoop 每个用户一个 goroutine 长轮询
func (m *WeChatManager) pollLoop(idx string, user *wechatUserState) {
	log.Printf("[WECHAT] 开始轮询用户 %s (%s)", idx, user.Route.WechatID)

	for {
		select {
		case <-user.stopCh:
			log.Printf("[WECHAT] 停止轮询用户 %s", idx)
			return
		default:
		}

		msgs, newBuf, err := user.Bot.GetUpdates()
		if err != nil {
			log.Printf("[WECHAT] GetUpdates error (%s): %v", idx, err)
			// 检查是否 session 失效
			if strings.Contains(err.Error(), "ret=2000") || strings.Contains(err.Error(), "ret=2001") {
				log.Printf("[WECHAT] 用户 %s session 失效，标记为需要重新登录", idx)
				m.mu.Lock()
				user.LoginResult = nil
				m.mu.Unlock()
				return
			}
			time.Sleep(5 * time.Second)
			continue
		}
		user.Bot.UpdateBuf = newBuf

		for _, msg := range msgs {
			// P1-a: 投递串行队列（processLoop 按序处理），非阻塞写，满则丢弃+日志
			select {
			case user.MsgQueue <- msg:
			default:
				log.Printf("[WECHAT] msgQueue full (user=%s), dropping message from %s", idx, msg.FromUserID)
			}
		}
	}
}

// processLoop 串行处理用户的微信消息（P1-a：保证顺序，避免 pollLoop 并发 go handleMessage 导致乱序/409 丢失）
func (m *WeChatManager) processLoop(idx string, user *wechatUserState) {
	for {
		select {
		case msg, ok := <-user.MsgQueue:
			if !ok {
				return
			}
			m.handleMessage(idx, user, msg)
		case <-user.stopCh:
			return
		}
	}
}

// handleMessage 处理收到的微信消息
func (m *WeChatManager) handleMessage(idx string, user *wechatUserState, msg ILinkIncomingMessage) {
	fromUser := msg.FromUserID
	text := msg.Text
	ctxToken := msg.ContextToken

	log.Printf("[WECHAT] 收到消息 [user=%s, from=%s]: %s", idx, fromUser, truncateStr(text, 80))

	// 白名单检查：只接受配置的 wechat_id
	if fromUser != user.Route.WechatID {
		log.Printf("[WECHAT] 拒绝非配置用户 %s (期望 %s)", fromUser, user.Route.WechatID)
		return
	}

	// 发送回复的辅助函数
	sendReply := func(replyText string) {
		if err := user.Bot.SendMessage(fromUser, replyText, ctxToken); err != nil {
			log.Printf("[WECHAT] 发送失败 [to=%s]: %v", fromUser, err)
		}
	}

	// 处理指令（/provider、/engine、/model 透传给 client，由 client 端处理）
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "/provider") && !strings.HasPrefix(text, "/engine") && !strings.HasPrefix(text, "/model") {
		m.handleCommand(idx, user, fromUser, text, sendReply)
		return
	}

	// 检查是否有绑定客户端
	clawbotID := user.Route.ClawbotID
	clientID, bound := user.Bindings[fromUser]

	if !bound {
		// 自动绑定：找到第一个该 clawbotID 下的在线 client
		conn, ok := m.hub.FindClientByClawbotID(clawbotID)
		if !ok {
			sendReply(fmt.Sprintf("❌ 电脑 %q 上没有在线的 Client", clawbotID))
			return
		}
		clientID = conn.ID
		user.Bindings[fromUser] = clientID
		m.saveBindings(idx, user)
	}

	// 检查客户端是否在线
	_, ok := m.hub.GetClient(clientID)
	if !ok {
		// 尝试重新绑定
		conn, found := m.hub.FindClientByClawbotID(clawbotID)
		if !found {
			sendReply(fmt.Sprintf("❌ 电脑 %q 上没有在线的 Client", clawbotID))
			return
		}
		clientID = conn.ID
		user.Bindings[fromUser] = clientID
		m.saveBindings(idx, user)
	}

	// 发送输入状态
	go m.sendTyping(user, fromUser)

	// 通过 Hub 路由消息（复用 BotHandler 的虚拟连接模式）
	resp, err := m.chatViaHub(clientID, text, fromUser)
	if err != nil {
		log.Printf("[WECHAT] Chat error: %v", err)
		sendReply(fmt.Sprintf("❌ 请求失败: %v", err))
		return
	}

	if resp.IsBackground {
		sendReply("⏳ 任务执行超时，已转为后台运行，完成后会推送结果给你")
		return
	}

	if resp.IsError {
		sendReply(fmt.Sprintf("❌ Claude 错误: %s", resp.ErrorMsg))
		return
	}

	reply := resp.FullText
	if reply == "" {
		reply = "（Claude 未返回文本内容）"
	}

	// 分段发送
	chunks := splitMessage(reply, 4000)
	for i, chunk := range chunks {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		if err := user.Bot.SendMessage(fromUser, chunk, ctxToken); err != nil {
			log.Printf("[WECHAT] 发送失败 [to=%s, chunk=%d]: %v", fromUser, i+1, err)
		}
	}

	log.Printf("[WECHAT] 回复已发送 [to=%s, chunks=%d]", fromUser, len(chunks))

	// 取消输入状态
	if ticket, ok := user.TypingTickets[fromUser]; ok {
		user.Bot.SendTyping(fromUser, ticket, 2)
	}
}

// wechatChatResponse Hub 聊天响应
type wechatChatResponse struct {
	FullText       string
	ToolCalls      []string
	CostUSD        float64
	IsError        bool
	ErrorMsg       string
	IsBackground   bool   // 超时转后台
	BgTaskID       string // 后台任务 ID
	hasStreamDelta bool
}

// collectText 以 result 的非空文本作为最终答案；此前的 text/stream_delta 仅作兼容回退。
// Claude 的一次任务可包含多段工具执行进度，不能让第一段进度覆盖最终 result。
func (r *wechatChatResponse) collectText(eventType, text string) {
	switch eventType {
	case "stream_delta":
		r.hasStreamDelta = true
		r.FullText += text
	case "text":
		if !r.hasStreamDelta {
			r.FullText = text
		}
	case "result":
		if strings.TrimSpace(text) != "" {
			r.FullText = text
		}
	}
}

// chatViaHub 通过 Hub 直接路由消息（不走 HTTP）
func (m *WeChatManager) chatViaHub(clientID string, text string, wechatID string) (*wechatChatResponse, error) {
	client, ok := m.hub.GetClient(clientID)
	if !ok {
		return nil, fmt.Errorf("client %s not found", clientID)
	}

	// 创建虚拟 bot 连接（ID 以 "bot-" 开头，让 Client 识别为 Bot API）
	botConn := &Connection{
		ID:   "bot-wechat-" + uuid.New().String(),
		Type: ConnTypeUser,
		Send: make(chan *protocol.Message, 256),
	}

	m.hub.RegisterBotUser(botConn)

	if !m.hub.AttachUser(botConn.ID, clientID) {
		m.hub.CleanupBotUser(botConn)
		return nil, fmt.Errorf("failed to attach to client")
	}

	// 后台模式标记：超时转后台时延迟清理，避免 Client 收到 bgMode 前的 in-flight 事件丢失
	wentBackground := false
	defer func() {
		if wentBackground {
			// 延迟 10 秒清理，给 Client 足够时间收到 bgMode 消息并停止发送事件
			go func() {
				time.Sleep(10 * time.Second)
				m.hub.DetachUser(botConn.ID)
				m.hub.CleanupBotUser(botConn)
			}()
		} else {
			m.hub.DetachUser(botConn.ID)
			m.hub.CleanupBotUser(botConn)
		}
	}()

	// 发送 attach 通知
	if !safeSend(client.Send, &protocol.Message{
		Type: protocol.TypeAttach,
		From: botConn.ID,
	}) {
		return nil, fmt.Errorf("client disconnected before attach")
	}

	// 发送 chat_input
	chatMsg, err := protocol.NewMessage(protocol.TypeChatInput, protocol.ChatInputPayload{
		Text:     text,
		WechatID: wechatID,
	})
	if err != nil {
		return nil, err
	}
	chatMsg.From = botConn.ID
	if !safeSend(client.Send, chatMsg) {
		return nil, fmt.Errorf("client disconnected before chat")
	}

	// 结束时 detach
	defer func() {
		safeSend(client.Send, &protocol.Message{
			Type: protocol.TypeDetach,
			From: botConn.ID,
		})
	}()

	// 收集响应
	result := &wechatChatResponse{}
	timeout := time.NewTimer(3 * time.Minute)
	defer timeout.Stop()
	hardTimeout := time.NewTimer(30 * time.Minute)
	defer hardTimeout.Stop()

	for {
		select {
		case msg, ok := <-botConn.Send:
			if !ok {
				return result, nil
			}

			switch msg.Type {
			case protocol.TypeChatMessage:
				var payload protocol.ChatMessagePayload
				if err := json.Unmarshal(msg.Payload, &payload); err != nil {
					continue
				}
				switch payload.EventType {
				case "stream_delta":
					result.collectText(payload.EventType, payload.Text)
					timeout.Reset(3 * time.Minute)
				case "text":
					result.collectText(payload.EventType, payload.Text)
					timeout.Reset(3 * time.Minute)
				case "result":
					result.collectText(payload.EventType, payload.Text)
					result.CostUSD = payload.CostUSD
				case "tool_start", "tool_end", "thinking":
					// 任何来自 Claude 的事件都说明进程活跃，应重置超时。
					// Claude Code 任务普遍包含长时间工具调用（Read/Bash/Agent 等），
					// 工具执行期间没有文字输出但进程仍在工作，必须 reset 否则误判超时。
					if payload.EventType == "tool_start" {
						result.ToolCalls = append(result.ToolCalls, payload.ToolName)
					}
					timeout.Reset(3 * time.Minute)
				}

			case protocol.TypeChatReady:
				return result, nil

			case protocol.TypeChatError:
				var payload protocol.ErrorPayload
				json.Unmarshal(msg.Payload, &payload)
				result.IsError = true
				result.ErrorMsg = payload.Message
				return result, nil
			}

		case <-timeout.C:
			wentBackground = true
			taskID := uuid.New().String()
			bgMsg, _ := protocol.NewMessage(protocol.TypeBackgroundMode, protocol.BackgroundModePayload{
				TaskID:      taskID,
				WechatID:    wechatID,
				RequesterID: botConn.ID,
			})
			safeSend(client.Send, bgMsg)
			log.Printf("[BG] Task timeout, switching to background: taskID=%s wechatID=%s", taskID, wechatID)
			return &wechatChatResponse{IsBackground: true, BgTaskID: taskID}, nil

		case <-hardTimeout.C:
			wentBackground = true
			taskID := uuid.New().String()
			bgMsg, _ := protocol.NewMessage(protocol.TypeBackgroundMode, protocol.BackgroundModePayload{
				TaskID:      taskID,
				WechatID:    wechatID,
				RequesterID: botConn.ID,
			})
			safeSend(client.Send, bgMsg)
			log.Printf("[BG] Hard timeout, switching to background: taskID=%s wechatID=%s", taskID, wechatID)
			return &wechatChatResponse{IsBackground: true, BgTaskID: taskID}, nil
		}
	}
}

// handleCommand 处理微信指令
func (m *WeChatManager) handleCommand(idx string, user *wechatUserState, fromUser, text string, sendReply func(string)) {
	parts := strings.Fields(text)
	cmd := parts[0]

	switch cmd {
	case "/clients":
		var clients []protocol.ClientInfo
		if user.Route.ClawbotID != "" {
			clients = m.hub.ListClientsByClawbotID(user.Route.ClawbotID)
		} else {
			clients = m.hub.ListClients()
		}

		if len(clients) == 0 {
			sendReply("没有在线的客户端")
			return
		}

		var sb strings.Builder
		sb.WriteString("在线客户端：\n")
		for i, c := range clients {
			active := ""
			if user.Bindings[fromUser] == c.ID {
				active = " ← 当前"
			}
			fmt.Fprintf(&sb, "%d. %s [%s]%s\n", i+1, c.Name, c.ClawbotID, active)
		}
		sb.WriteString("\n切换: /switch <序号>")
		sendReply(sb.String())

	case "/switch":
		if len(parts) < 2 {
			sendReply("用法: /switch <序号或client_id>")
			return
		}
		target := parts[1]

		// 查找客户端列表，支持序号切换
		var targetID string
		if clients := m.hub.ListClientsByClawbotID(user.Route.ClawbotID); len(clients) > 0 {
			// 序号
			for i, c := range clients {
				if fmt.Sprintf("%d", i+1) == target {
					targetID = c.ID
					break
				}
			}
			// 或直接 client_id
			if targetID == "" {
				for _, c := range clients {
					if c.ID == target {
						targetID = c.ID
						break
					}
				}
			}
		}

		if targetID == "" {
			sendReply(fmt.Sprintf("❌ 未找到客户端: %s", target))
			return
		}

		user.Bindings[fromUser] = targetID
		m.saveBindings(idx, user)
		sendReply(fmt.Sprintf("✅ 已切换到 %s", targetID))

	case "/new":
		clientID, bound := user.Bindings[fromUser]
		if !bound || clientID == "" {
			sendReply("✅ 新会话指令已记录（下一条消息将使用新会话）")
			return
		}
		client, ok := m.hub.GetClient(clientID)
		if !ok {
			sendReply("❌ 客户端不在线")
			return
		}
		newMsg, _ := protocol.NewMessage(protocol.TypeNewSession, nil)
		newMsg.From = "bot-wechat-" + fromUser
		if !safeSend(client.Send, newMsg) {
			sendReply("❌ 发送失败，客户端可能已断开")
			return
		}
		sendReply("✅ 已新建会话，请发送你的消息")

	case "/status":
		binding := user.Bindings[fromUser]
		if binding == "" {
			sendReply("当前无活跃会话")
			return
		}
		sendReply(fmt.Sprintf("当前绑定：\n- Clawbot: %s\n- Client: %s", user.Route.ClawbotID, binding))

	default:
		sendReply("未知指令。可用指令：/clients /switch <序号> /new /status /provider")
	}
}

// sendTyping 发送输入状态指示
func (m *WeChatManager) sendTyping(user *wechatUserState, wechatUserID string) {
	ticket, ok := user.TypingTickets[wechatUserID]
	if !ok {
		t, err := user.Bot.GetConfig(wechatUserID)
		if err == nil && t != "" {
			user.TypingTickets[wechatUserID] = t
			ticket = t
		}
	}
	if ticket != "" {
		user.Bot.SendTyping(wechatUserID, ticket, 1)
	}
}

// --- 持久化 ---

func (m *WeChatManager) userDir(idx string) string {
	return filepath.Join(m.dataDir, idx)
}

func (m *WeChatManager) saveSession(idx string, result *ILinkLoginResult) {
	dir := m.userDir(idx)
	os.MkdirAll(dir, 0755)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Printf("[WECHAT] 序列化 session 失败: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), data, 0600); err != nil {
		log.Printf("[WECHAT] 保存 session 失败: %v", err)
	}
}

func (m *WeChatManager) loadSession(idx string) *ILinkLoginResult {
	data, err := os.ReadFile(filepath.Join(m.userDir(idx), "session.json"))
	if err != nil {
		return nil
	}
	var result ILinkLoginResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	if result.BotToken == "" {
		return nil
	}
	return &result
}

func (m *WeChatManager) saveBindings(idx string, user *wechatUserState) {
	dir := m.userDir(idx)
	os.MkdirAll(dir, 0755)

	data, err := json.MarshalIndent(user.Bindings, "", "  ")
	if err != nil {
		log.Printf("[WECHAT] 序列化 bindings 失败: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "bindings.json"), data, 0644); err != nil {
		log.Printf("[WECHAT] 保存 bindings 失败: %v", err)
	}
}

func (m *WeChatManager) loadBindings(idx string, user *wechatUserState) {
	data, err := os.ReadFile(filepath.Join(m.userDir(idx), "bindings.json"))
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &user.Bindings); err != nil {
		return
	}
	log.Printf("[WECHAT] 恢复 %d 个绑定", len(user.Bindings))
}

// --- 工具函数 ---

// safeSend 安全发送消息到 channel，避免向已关闭 channel 写入导致 panic
func safeSend(ch chan *protocol.Message, msg *protocol.Message) bool {
	defer func() { recover() }()
	ch <- msg
	return true
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func splitMessage(text string, chunkSize int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= chunkSize {
			chunks = append(chunks, text)
			break
		}

		cut := findSafeCutPoint(text, chunkSize)
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}

	// 修复被切割的代码块：如果 chunk 有未闭合的 ```，补上闭合标记，下一段补上重开标记
	for i := 0; i < len(chunks)-1; i++ {
		if hasUnclosedCodeBlock(chunks[i]) {
			lang := extractCodeBlockLang(chunks[i])
			chunks[i] += "\n```\n"
			if !strings.HasPrefix(strings.TrimSpace(chunks[i+1]), "```") {
				if lang != "" {
					chunks[i+1] = "```" + lang + "\n" + chunks[i+1]
				} else {
					chunks[i+1] = "```\n" + chunks[i+1]
				}
			}
		}
	}

	// 不再添加 [i/n] 前缀：前缀会让 chunk 行首的 md 标记（#/-/|/>/```）失效，
	// 同时让上面的代码块修复成果失效（``` 不再在行首）。
	// 多段消息天然按时间排序到达，用户能感知顺序，不需要序号提示。
	return chunks
}

// findSafeCutPoint 在 maxBytes 范围内找到一个安全的切割点
// 优先在段落/空行处切割，其次在句子结尾，最后在 UTF-8 字符边界
// 同时避免切割在代码块或表格中间
func findSafeCutPoint(text string, maxBytes int) int {
	if maxBytes >= len(text) {
		return len(text)
	}

	codeBlocks := findCodeBlockRanges(text)

	// adjust: 如果候选切割点在代码块内，尝试移到代码块之后或之前
	adjust := func(cut int) int {
		for _, cb := range codeBlocks {
			if cut > cb[0] && cut < cb[1] {
				// 优先移到代码块末尾（不超过 maxBytes 的 150%）
				if cb[1] <= maxBytes+(maxBytes/2) {
					return cb[1]
				}
				// 代码块太长，移到代码块开头之前
				if cb[0] > 0 {
					return cb[0]
				}
				return 0 // 放弃此候选点
			}
		}
		return cut
	}

	// 1. 优先在段落边界（空行 \n\n）切割
	if idx := lastIndexOf(text[:maxBytes], "\n\n"); idx > 0 {
		if cut := adjust(idx + 2); cut > 0 {
			return cut
		}
	}

	// 2. 检查切割点是否在表格中间，如果是则回退到表格开始之前
	if cut := avoidTableCut(text, maxBytes); cut > 0 {
		if cut := adjust(cut); cut > 0 {
			return cut
		}
	}

	// 3. 在换行符处切割
	if idx := lastIndexOf(text[:maxBytes], "\n"); idx > 0 {
		if cut := adjust(idx + 1); cut > 0 {
			return cut
		}
	}

	// 4. 在句号、感叹号、问号等句子结尾切割（中文和英文）
	if idx := lastIndexOfAny(text[:maxBytes], "。！？.!?\n"); idx > 0 {
		// 跳过句末标点，在后面切割
		_, size := utf8.DecodeRuneInString(text[idx:])
		if cut := adjust(idx + size); cut > 0 {
			return cut
		}
	}

	// 5. 兜底：确保在 UTF-8 字符边界切割
	pos := maxBytes
	for pos > 0 && !utf8.RuneStart(text[pos]) {
		pos--
	}
	return pos
}

// lastIndexOf 返回 s 中 substr 最后一次出现的位置
func lastIndexOf(s, substr string) int {
	return strings.LastIndex(s, substr)
}

// lastIndexOfAny 返回 s 中 chars 任意字符最后一次出现的位置
func lastIndexOfAny(s, chars string) int {
	return strings.LastIndexAny(s, chars)
}

// avoidTableCut 检查 maxBytes 位置的行是否在 Markdown 表格中间
// 如果是，返回表格开始之前的切割位置；如果不是表格中间，返回 -1
func avoidTableCut(text string, maxBytes int) int {
	// 找到 maxBytes 位置所在的行
	lineStart := lastIndexOf(text[:maxBytes], "\n")
	if lineStart < 0 {
		lineStart = 0
	} else {
		lineStart++ // 跳过 \n
	}

	// 找到行结束位置
	lineEnd := strings.Index(text[maxBytes:], "\n")
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd = maxBytes + lineEnd
	}

	// 检查当前行是否是表格行（以 | 开头）
	line := strings.TrimSpace(text[lineStart:lineEnd])
	if line == "" || line[0] != '|' {
		return -1 // 不在表格行上
	}

	// 当前在表格中间，需要找到这个表格块的起始位置
	// 从 lineStart 往前找，直到遇到非表格行或文本开头
	lines := strings.Split(text[:lineStart], "\n")
	tableStartLine := len(lines) - 1
	for tableStartLine >= 0 {
		trimmed := strings.TrimSpace(lines[tableStartLine])
		if trimmed == "" || trimmed[0] != '|' {
			tableStartLine++
			break
		}
		tableStartLine--
	}
	if tableStartLine < 0 {
		tableStartLine = 0
	}

	// 计算表格开始前的字节偏移
	if tableStartLine == 0 {
		return 0 // 表格从文本开头就开始了，无法回退
	}

	// 累加到 tableStartLine-1 行末尾的字节偏移（在表格前的换行处切割）
	offset := 0
	for i := 0; i < tableStartLine; i++ {
		offset += len(lines[i])
		if i < len(lines)-1 {
			offset++ // \n
		}
	}
	if offset > 0 {
		return offset + 1 // +1 跳过 \n
	}
	return -1
}

// findCodeBlockRanges 返回所有围栏代码块的 [start, end) 字节偏移范围
func findCodeBlockRanges(text string) [][2]int {
	var ranges [][2]int
	inCode := false
	codeStart := 0
	lineStart := 0

	for i := 0; i <= len(text); i++ {
		if i == len(text) || text[i] == '\n' {
			trimmed := strings.TrimSpace(text[lineStart:i])
			if !inCode && strings.HasPrefix(trimmed, "```") {
				inCode = true
				codeStart = lineStart
			} else if inCode && strings.HasPrefix(trimmed, "```") {
				inCode = false
				end := i
				if end < len(text) {
					end++ // include \n
				}
				ranges = append(ranges, [2]int{codeStart, end})
			}
			lineStart = i + 1
		}
	}

	if inCode {
		ranges = append(ranges, [2]int{codeStart, len(text)})
	}

	return ranges
}

// hasUnclosedCodeBlock 检查文本中是否有未闭合的围栏代码块
func hasUnclosedCodeBlock(text string) bool {
	inCode := false
	lineStart := 0
	for i := 0; i <= len(text); i++ {
		if i == len(text) || text[i] == '\n' {
			if strings.HasPrefix(strings.TrimSpace(text[lineStart:i]), "```") {
				inCode = !inCode
			}
			lineStart = i + 1
		}
	}
	return inCode
}

// extractCodeBlockLang 提取最后一个未闭合代码块的语言标记
func extractCodeBlockLang(text string) string {
	inCode := false
	lang := ""
	lineStart := 0
	for i := 0; i <= len(text); i++ {
		if i == len(text) || text[i] == '\n' {
			trimmed := strings.TrimSpace(text[lineStart:i])
			if strings.HasPrefix(trimmed, "```") {
				inCode = !inCode
				if inCode {
					lang = strings.TrimSpace(trimmed[3:])
				}
			}
			lineStart = i + 1
		}
	}
	return lang
}

// --- Push API ---

// pushQueueItem Push 离线队列条目
type pushQueueItem struct {
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// PushMessage 向指定微信用户推送消息
// 返回 "sent" 表示立即发送成功，"queued" 表示已入队等待登录后发送
func (m *WeChatManager) PushMessage(wechatID, text string) (string, error) {
	m.mu.Lock()

	// 查找 wechat_id 对应的 user state
	var foundIdx string
	var foundUser *wechatUserState
	for idx, user := range m.users {
		if user.Route.WechatID == wechatID {
			foundIdx = idx
			foundUser = user
			break
		}
	}

	if foundUser == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("wechat_id %q not in config", wechatID)
	}

	// 未登录：入队（持锁操作，无网络 IO）
	if foundUser.LoginResult == nil {
		foundUser.PushQueue = append(foundUser.PushQueue, pushQueueItem{
			Text:      text,
			CreatedAt: time.Now(),
		})
		m.savePushQueue(foundIdx, foundUser)
		log.Printf("[PUSH] 消息已入队 to=%s queue_len=%d", wechatID, len(foundUser.PushQueue))
		m.mu.Unlock()
		return "queued", nil
	}

	// 已登录：提取需要的信息后释放锁，再执行网络 IO
	bot := foundUser.Bot
	chunks := splitMessage(text, 4000)
	m.mu.Unlock()

	// 在锁外执行网络发送
	sentCount := 0
	for i, chunk := range chunks {
		if err := bot.SendMessage(wechatID, chunk, ""); err != nil {
			log.Printf("[PUSH] 发送失败 to=%s chunk=%d/%d: %v，剩余入队", wechatID, i+1, len(chunks), err)
			// 发送失败，将剩余未发送内容入队
			remaining := strings.Join(chunks[i:], "")
			m.mu.Lock()
			foundUser.PushQueue = append(foundUser.PushQueue, pushQueueItem{
				Text:      remaining,
				CreatedAt: time.Now(),
			})
			m.savePushQueue(foundIdx, foundUser)
			m.mu.Unlock()
			return "queued", nil
		}
		sentCount++
		time.Sleep(500 * time.Millisecond)
	}

	log.Printf("[PUSH] 消息已发送 to=%s len=%d chunks=%d", wechatID, len(text), sentCount)
	return "sent", nil
}

// flushPushQueue 投递离线队列中的消息
func (m *WeChatManager) flushPushQueue(idx string, user *wechatUserState) bool {
	user.pushMu.Lock()
	defer user.pushMu.Unlock()
	m.mu.Lock()
	if len(user.PushQueue) == 0 {
		m.mu.Unlock()
		return true
	}
	queue := append([]pushQueueItem(nil), user.PushQueue...)
	bot := user.Bot
	wechatID := user.Route.WechatID
	m.mu.Unlock()

	log.Printf("[PUSH] 投递离线队列 user=%s count=%d", idx, len(queue))

	for i, item := range queue {
		chunks := splitMessage(item.Text, 4000)
		for _, chunk := range chunks {
			if err := bot.SendMessage(wechatID, chunk, ""); err != nil {
				log.Printf("[PUSH] 投递失败 [user=%s, item=%d]: %v", idx, i, err)
				// 发送失败，保留剩余消息
				m.mu.Lock()
				newItems := append([]pushQueueItem(nil), user.PushQueue[len(queue):]...)
				user.PushQueue = append(queue[i:], newItems...)
				m.savePushQueue(idx, user)
				m.mu.Unlock()
				return false
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// 全部发送成功，清空队列
	m.mu.Lock()
	user.PushQueue = user.PushQueue[len(queue):]
	m.savePushQueue(idx, user)
	m.mu.Unlock()
	log.Printf("[PUSH] 离线队列已清空 user=%s", idx)
	return true
}

// savePushQueue 持久化 Push 队列
func (m *WeChatManager) savePushQueue(idx string, user *wechatUserState) {
	dir := m.userDir(idx)
	os.MkdirAll(dir, 0755)

	data, err := json.MarshalIndent(user.PushQueue, "", "  ")
	if err != nil {
		log.Printf("[WECHAT] 序列化 push queue 失败: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "push_queue.json"), data, 0600); err != nil {
		log.Printf("[WECHAT] 保存 push queue 失败: %v", err)
	}
}

// loadPushQueue 加载 Push 队列
func (m *WeChatManager) loadPushQueue(idx string, user *wechatUserState) {
	data, err := os.ReadFile(filepath.Join(m.userDir(idx), "push_queue.json"))
	if err != nil {
		return
	}
	var queue []pushQueueItem
	if err := json.Unmarshal(data, &queue); err != nil {
		return
	}
	user.PushQueue = queue
	if len(queue) > 0 {
		log.Printf("[WECHAT] 恢复 %d 条待推送消息 (%s)", len(queue), idx)
	}
}

// saveBuf 持久化 UpdateBuf（P2：服务端正常重启时落盘，重启后游标不归零，避免 iLink 重投递断线期间消息）
func (m *WeChatManager) saveBuf(idx string, user *wechatUserState) {
	dir := m.userDir(idx)
	os.MkdirAll(dir, 0755)
	if err := os.WriteFile(filepath.Join(dir, "update_buf.txt"), []byte(user.Bot.UpdateBuf), 0600); err != nil {
		log.Printf("[WECHAT] 保存 update_buf 失败: %v", err)
	}
}

// loadBuf 恢复 UpdateBuf
func (m *WeChatManager) loadBuf(idx string, user *wechatUserState) {
	data, err := os.ReadFile(filepath.Join(m.userDir(idx), "update_buf.txt"))
	if err != nil {
		return
	}
	user.Bot.UpdateBuf = string(data)
	if len(data) > 0 {
		log.Printf("[WECHAT] 恢复 update_buf (%s): %d 字节", idx, len(data))
	}
}

// IsWechatIDInConfig 检查 wechat_id 是否在配置的白名单中，并返回该用户的 push_secret
func (m *WeChatManager) IsWechatIDInConfig(wechatID string) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range m.users {
		if user.Route.WechatID == wechatID {
			return true, user.Route.PushSecret
		}
	}
	return false, ""
}
