package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/shayin/claude-forward/internal/protocol"
)

// feishuIncomingMessage 串行队列条目（保证同一用户消息按发送序处理）
type feishuIncomingMessage struct {
	route  *FeishuUserRoute
	openID string
	text   string
}

// FeishuManager 飞书接入管理器
// 一个自建应用机器人（AppID/AppSecret）服务多个白名单 open_id 用户，
// 每个 open_id 通过 clawbot_id 路由到对应的 client。
type FeishuManager struct {
	hub       *Hub
	auth      *Auth
	config    FeishuConfig
	apiClient *lark.Client // 发消息用
	wsClient  *larkws.Client

	routes   map[string]*FeishuUserRoute // 配置序号 → route
	openMap  map[string]*FeishuUserRoute // open_id → route（白名单 + 路由）
	bindings map[string]string           // open_id → clientID
	dataDir  string

	mu       sync.Mutex
	msgQueue chan *feishuIncomingMessage
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewFeishuManager 创建飞书管理器
func NewFeishuManager(hub *Hub, auth *Auth, cfg FeishuConfig) *FeishuManager {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "feishu-data"
	}

	routes := make(map[string]*FeishuUserRoute)
	openMap := make(map[string]*FeishuUserRoute)
	for i := range cfg.Users {
		route := &cfg.Users[i]
		routes[fmt.Sprintf("%d", i)] = route
		if route.FeishuID != "" {
			openMap[route.FeishuID] = route
		}
	}

	return &FeishuManager{
		hub:       hub,
		auth:      auth,
		config:    cfg,
		apiClient: lark.NewClient(cfg.AppID, cfg.AppSecret),
		routes:    routes,
		openMap:   openMap,
		bindings:  make(map[string]string),
		dataDir:   dataDir,
		msgQueue:  make(chan *feishuIncomingMessage, 64),
	}
}

// Start 启动飞书长连接
func (m *FeishuManager) Start() {
	// 恢复 bindings
	m.loadBindings()

	// 串行消息处理 goroutine
	go m.processLoop()

	// 事件分发器
	eventDispatcher := dispatcher.NewEventDispatcher("", "")
	eventDispatcher.OnP2MessageReceiveV1(func(ctx context.Context, e *larkim.P2MessageReceiveV1) error {
		m.enqueueMessage(e)
		return nil
	})

	// 长连接 client（SDK 自动管理 token、重连）
	m.wsClient = larkws.NewClient(m.config.AppID, m.config.AppSecret,
		larkws.WithEventHandler(eventDispatcher),
		larkws.WithAutoReconnect(true),
	)

	m.ctx, m.cancel = context.WithCancel(context.Background())

	go func() {
		log.Printf("[FEISHU] 启动长连接 appID=%s", m.config.AppID)
		if err := m.wsClient.Start(m.ctx); err != nil && err != context.Canceled {
			log.Printf("[FEISHU] 长连接退出: %v", err)
		}
	}()
}

// Stop 停止长连接
func (m *FeishuManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	log.Println("[FEISHU] 已停止")
}

// enqueueMessage 将收到的飞书消息投递到串行队列
func (m *FeishuManager) enqueueMessage(e *larkim.P2MessageReceiveV1) {
	if e.Event == nil || e.Event.Message == nil || e.Event.Sender == nil {
		return
	}
	msg := e.Event.Message
	if msg.MessageType == nil || *msg.MessageType != "text" {
		// 非文本消息暂不处理
		return
	}
	if msg.Content == nil {
		return
	}
	sender := e.Event.Sender
	if sender.SenderId == nil || sender.SenderId.OpenId == nil {
		return
	}
	openID := *sender.SenderId.OpenId

	// 解析文本（飞书 text 消息 content 为 {"text":"..."} JSON 字符串）
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*msg.Content), &c); err != nil {
		log.Printf("[FEISHU] 解析消息内容失败: %v", err)
		return
	}
	text := strings.TrimSpace(c.Text)
	if text == "" {
		return
	}

	// 白名单检查
	route, ok := m.openMap[openID]
	if !ok {
		log.Printf("[FEISHU] 拒绝非白名单用户 %s", openID)
		return
	}

	incoming := &feishuIncomingMessage{route: route, openID: openID, text: text}
	select {
	case m.msgQueue <- incoming:
	default:
		log.Printf("[FEISHU] msgQueue full, dropping message from %s", openID)
	}
}

// processLoop 串行处理飞书消息（避免并发抢写同一 client 导致顺序错乱）
func (m *FeishuManager) processLoop() {
	for {
		select {
		case msg, ok := <-m.msgQueue:
			if !ok {
				return
			}
			m.handleMessage(msg.route, msg.openID, msg.text)
		case <-m.ctx.Done():
			return
		}
	}
}

// handleMessage 处理一条飞书消息
func (m *FeishuManager) handleMessage(route *FeishuUserRoute, openID, text string) {
	log.Printf("[FEISHU] 收到消息 [from=%s]: %s", openID, truncateStr(text, 80))

	sendReply := func(replyText string) {
		if err := m.sendText(openID, replyText); err != nil {
			log.Printf("[FEISHU] 发送失败 [to=%s]: %v", openID, err)
		}
	}

	// 处理指令（/provider、/engine、/model 透传给 client，由 client 端处理）
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "/provider") && !strings.HasPrefix(text, "/engine") && !strings.HasPrefix(text, "/model") {
		m.handleCommand(route, openID, text, sendReply)
		return
	}

	// 检查绑定客户端
	clawbotID := route.ClawbotID
	clientID := m.getBinding(openID)
	if clientID == "" {
		conn, ok := m.hub.FindClientByClawbotID(clawbotID)
		if !ok {
			sendReply(fmt.Sprintf("❌ 电脑 %q 上没有在线的 Client", clawbotID))
			return
		}
		clientID = conn.ID
		m.setBinding(openID, clientID)
	}

	// 检查客户端是否在线
	if _, ok := m.hub.GetClient(clientID); !ok {
		conn, found := m.hub.FindClientByClawbotID(clawbotID)
		if !found {
			sendReply(fmt.Sprintf("❌ 电脑 %q 上没有在线的 Client", clawbotID))
			return
		}
		clientID = conn.ID
		m.setBinding(openID, clientID)
	}

	// 通过 Hub 路由消息
	resp, err := m.chatViaHub(clientID, text, openID)
	if err != nil {
		log.Printf("[FEISHU] Chat error: %v", err)
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
		if err := m.sendText(openID, chunk); err != nil {
			log.Printf("[FEISHU] 发送失败 [to=%s, chunk=%d]: %v", openID, i+1, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("[FEISHU] 回复已发送 [to=%s, chunks=%d]", openID, len(chunks))
}

// feishuChatResponse Hub 聊天响应
type feishuChatResponse struct {
	FullText       string
	ToolCalls      []string
	CostUSD        float64
	IsError        bool
	ErrorMsg       string
	IsBackground   bool
	BgTaskID       string
	hasStreamDelta bool
}

// collectText 以 result 的非空文本作为最终答案；此前的 text/stream_delta 仅作兼容回退。
func (r *feishuChatResponse) collectText(eventType, text string) {
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

// chatViaHub 通过 Hub 直接路由消息（逻辑复用自 WeChatManager.chatViaHub）
// BackgroundModePayload.WechatID 字段复用为 open_id（client 透传，后台结果据此回推）。
func (m *FeishuManager) chatViaHub(clientID string, text string, openID string) (*feishuChatResponse, error) {
	client, ok := m.hub.GetClient(clientID)
	if !ok {
		return nil, fmt.Errorf("client %s not found", clientID)
	}

	// 创建虚拟 bot 连接（ID 以 "bot-" 开头，让 Client 识别为 Bot API）
	botConn := &Connection{
		ID:   "bot-feishu-" + uuid.New().String(),
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
		WechatID: openID, // 复用 WechatID 字段携带 open_id
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
	result := &feishuChatResponse{}
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
				TaskID:   taskID,
				WechatID: openID,
			})
			safeSend(client.Send, bgMsg)
			log.Printf("[BG] Task timeout, switching to background: taskID=%s openID=%s", taskID, openID)
			return &feishuChatResponse{IsBackground: true, BgTaskID: taskID}, nil

		case <-hardTimeout.C:
			wentBackground = true
			taskID := uuid.New().String()
			bgMsg, _ := protocol.NewMessage(protocol.TypeBackgroundMode, protocol.BackgroundModePayload{
				TaskID:   taskID,
				WechatID: openID,
			})
			safeSend(client.Send, bgMsg)
			log.Printf("[BG] Hard timeout, switching to background: taskID=%s openID=%s", taskID, openID)
			return &feishuChatResponse{IsBackground: true, BgTaskID: taskID}, nil
		}
	}
}

// handleCommand 处理飞书指令
func (m *FeishuManager) handleCommand(route *FeishuUserRoute, openID, text string, sendReply func(string)) {
	parts := strings.Fields(text)
	cmd := parts[0]

	switch cmd {
	case "/clients":
		var clients []protocol.ClientInfo
		if route.ClawbotID != "" {
			clients = m.hub.ListClientsByClawbotID(route.ClawbotID)
		} else {
			clients = m.hub.ListClients()
		}

		if len(clients) == 0 {
			sendReply("没有在线的客户端")
			return
		}

		current := m.getBinding(openID)
		var sb strings.Builder
		sb.WriteString("在线客户端：\n")
		for i, c := range clients {
			active := ""
			if current == c.ID {
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

		var targetID string
		if clients := m.hub.ListClientsByClawbotID(route.ClawbotID); len(clients) > 0 {
			for i, c := range clients {
				if fmt.Sprintf("%d", i+1) == target {
					targetID = c.ID
					break
				}
			}
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

		m.setBinding(openID, targetID)
		sendReply(fmt.Sprintf("✅ 已切换到 %s", targetID))

	case "/new":
		clientID := m.getBinding(openID)
		if clientID == "" {
			sendReply("✅ 新会话指令已记录（下一条消息将使用新会话）")
			return
		}
		client, ok := m.hub.GetClient(clientID)
		if !ok {
			sendReply("❌ 客户端不在线")
			return
		}
		newMsg, _ := protocol.NewMessage(protocol.TypeNewSession, nil)
		newMsg.From = "bot-feishu-" + openID
		if !safeSend(client.Send, newMsg) {
			sendReply("❌ 发送失败，客户端可能已断开")
			return
		}
		sendReply("✅ 已新建会话，请发送你的消息")

	case "/status":
		binding := m.getBinding(openID)
		if binding == "" {
			sendReply("当前无活跃会话")
			return
		}
		sendReply(fmt.Sprintf("当前绑定：\n- Clawbot: %s\n- Client: %s", route.ClawbotID, binding))

	default:
		sendReply("未知指令。可用指令：/clients /switch <序号> /new /status /provider")
	}
}

// --- Push API ---

// PushMessage 向指定飞书用户推送消息
// 返回 "sent" 表示发送成功，"" + err 表示失败。
func (m *FeishuManager) PushMessage(openID, text string) (string, error) {
	m.mu.Lock()
	_, ok := m.openMap[openID]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("open_id %q not in config", openID)
	}

	chunks := splitMessage(text, 4000)
	for i, chunk := range chunks {
		if err := m.sendText(openID, chunk); err != nil {
			log.Printf("[PUSH] 发送失败 to=%s chunk=%d/%d: %v", openID, i+1, len(chunks), err)
			return "", err
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Printf("[PUSH] 消息已发送 to=%s chunks=%d", openID, len(chunks))
	return "sent", nil
}

// --- 飞书 API ---

// sendText 发送文本消息
func (m *FeishuManager) sendText(openID, text string) error {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("open_id").
		Body(&larkim.CreateMessageReqBody{
			ReceiveId: larkcore.StringPtr(openID),
			MsgType:   larkcore.StringPtr(larkim.MsgTypeText),
			Content:   larkcore.StringPtr(string(content)),
		}).
		Build()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := m.apiClient.Im.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("feishu create message failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// --- bindings 持久化 ---

func (m *FeishuManager) getBinding(openID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bindings[openID]
}

func (m *FeishuManager) setBinding(openID, clientID string) {
	m.mu.Lock()
	m.bindings[openID] = clientID
	m.mu.Unlock()
	m.saveBindings()
}

func (m *FeishuManager) saveBindings() {
	m.mu.Lock()
	data, err := json.MarshalIndent(m.bindings, "", "  ")
	m.mu.Unlock()
	if err != nil {
		log.Printf("[FEISHU] 序列化 bindings 失败: %v", err)
		return
	}
	os.MkdirAll(m.dataDir, 0755)
	if err := os.WriteFile(filepath.Join(m.dataDir, "bindings.json"), data, 0644); err != nil {
		log.Printf("[FEISHU] 保存 bindings 失败: %v", err)
	}
}

func (m *FeishuManager) loadBindings() {
	data, err := os.ReadFile(filepath.Join(m.dataDir, "bindings.json"))
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := json.Unmarshal(data, &m.bindings); err != nil {
		return
	}
	log.Printf("[FEISHU] 恢复 %d 个绑定", len(m.bindings))
}
