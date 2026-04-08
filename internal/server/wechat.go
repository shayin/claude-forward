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
	Bindings    map[string]string // wechatUserID → clientID
	TypingTickets map[string]string // wechatUserID → typingTicket

	// 登录流程状态
	QRPending   bool
	QRToken     string
	QRCodeURL   string
	QRDeadline  time.Time

	// 控制
	stopCh     chan struct{}
	stopped    bool
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
			Route:       route,
			Bot:         NewILinkBot(),
			Bindings:    make(map[string]string),
			TypingTickets: make(map[string]string),
			stopCh:      make(chan struct{}),
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
	for idx, user := range m.users {
		// 尝试加载已保存的 session
		session := m.loadSession(idx)
		if session != nil {
			user.Bot.Token = session.BotToken
			user.Bot.BaseURL = session.BaseURL
			if user.Bot.ValidateSession() {
				user.LoginResult = session
				log.Printf("[WECHAT] 用户 %s (%s) 恢复登录成功", idx, user.Route.WechatID)
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

// Stop 停止所有会话
func (m *WeChatManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for idx, user := range m.users {
		if !user.stopped && user.LoginResult != nil {
			close(user.stopCh)
			user.stopped = true
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
			go m.handleMessage(idx, user, msg)
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

	// 处理指令
	if strings.HasPrefix(text, "/") {
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
	resp, err := m.chatViaHub(clientID, text)
	if err != nil {
		log.Printf("[WECHAT] Chat error: %v", err)
		sendReply(fmt.Sprintf("❌ 请求失败: %v", err))
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
	FullText string
	ToolCalls []string
	CostUSD  float64
	IsError  bool
	ErrorMsg string
}

// chatViaHub 通过 Hub 直接路由消息（不走 HTTP）
func (m *WeChatManager) chatViaHub(clientID string, text string) (*wechatChatResponse, error) {
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
	defer m.hub.CleanupBotUser(botConn)

	if !m.hub.AttachUser(botConn.ID, clientID) {
		return nil, fmt.Errorf("failed to attach to client")
	}
	defer m.hub.DetachUser(botConn.ID)

	// 发送 attach 通知
	client.Send <- &protocol.Message{
		Type: protocol.TypeAttach,
		From: botConn.ID,
	}

	// 发送 chat_input
	chatMsg, err := protocol.NewMessage(protocol.TypeChatInput, protocol.ChatInputPayload{
		Text: text,
	})
	if err != nil {
		return nil, err
	}
	chatMsg.From = botConn.ID
	client.Send <- chatMsg

	// 结束时 detach
	defer func() {
		client.Send <- &protocol.Message{
			Type: protocol.TypeDetach,
			From: botConn.ID,
		}
	}()

	// 收集响应
	result := &wechatChatResponse{}
	timeout := time.NewTimer(5 * time.Minute)
	defer timeout.Stop()

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
					// 增量文本片段，累加
					result.FullText += payload.Text
				case "text":
					// 跳过：assistant 消息中的完整文本块，与 stream_delta 重复
				case "result":
					// 跳过：完整结果文本，stream_delta 已累加
					result.CostUSD = payload.CostUSD
				case "tool_start":
					result.ToolCalls = append(result.ToolCalls, payload.ToolName)
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

			timeout.Reset(5 * time.Minute)

		case <-timeout.C:
			return nil, fmt.Errorf("chat timeout")

		case <-time.After(6 * time.Minute):
			return nil, fmt.Errorf("chat timeout (hard)")
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
			sb.WriteString(fmt.Sprintf("%d. %s [%s]%s\n", i+1, c.Name, c.ClawbotID, active))
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
		sendReply("✅ 新会话指令已记录（下一条消息将使用新会话）")

	case "/status":
		binding := user.Bindings[fromUser]
		if binding == "" {
			sendReply("当前无活跃会话")
			return
		}
		sendReply(fmt.Sprintf("当前绑定：\n- Clawbot: %s\n- Client: %s", user.Route.ClawbotID, binding))

	default:
		sendReply("未知指令。可用指令：/clients /switch <序号> /new /status")
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
		chunks = append(chunks, text[:chunkSize])
		text = text[chunkSize:]
	}
	if len(chunks) > 1 {
		for i := range chunks {
			chunks[i] = fmt.Sprintf("[%d/%d] %s", i+1, len(chunks), chunks[i])
		}
	}
	return chunks
}
