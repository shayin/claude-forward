package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shayin/claude-forward/internal/protocol"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许所有来源
	},
}

const (
	backgroundResultTTL       = 5 * time.Minute
	backgroundDedupRetention  = 10 * time.Minute
	backgroundMaxClockSkew    = time.Minute
	backgroundPushesStateFile = "background-pushed.json"
)

// Handler WebSocket 处理器
type Handler struct {
	hub           *Hub
	auth          *Auth
	encryptionKey []byte         // 应用层加密密钥（nil 表示不加密）
	wechatMgr     *WeChatManager // 微信管理器（可选，用于后台任务推送）
	bgPushedMu    sync.Mutex
	bgPushed      map[string]int64 // taskID → 推送时间(UnixMilli)，LRU 去重防断线堆积重连洪泛
	bgPushedPath  string           // 已推送任务 ID 的持久化状态，跨服务端重启去重
	shareBroker   *shareRequestBroker
}

// NewHandler 创建处理器
func NewHandler(hub *Hub, auth *Auth, encryptionKey []byte) *Handler {
	return &Handler{
		hub:           hub,
		auth:          auth,
		encryptionKey: encryptionKey,
		bgPushed:      make(map[string]int64),
		shareBroker:   newShareRequestBroker(),
	}
}

// SetWeChatManager 设置微信管理器
func (h *Handler) SetWeChatManager(mgr *WeChatManager) {
	h.wechatMgr = mgr
	if mgr == nil {
		return
	}

	h.bgPushedMu.Lock()
	h.bgPushedPath = filepath.Join(mgr.dataDir, backgroundPushesStateFile)
	h.loadBackgroundPushesLocked(time.Now())
	h.bgPushedMu.Unlock()
}

func (h *Handler) loadBackgroundPushesLocked(now time.Time) {
	data, err := os.ReadFile(h.bgPushedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[BG] Failed to read pushed-task state: %v", err)
		}
		return
	}

	var persisted map[string]int64
	if err := json.Unmarshal(data, &persisted); err != nil {
		log.Printf("[BG] Failed to parse pushed-task state: %v", err)
		return
	}
	for taskID, pushedAt := range persisted {
		if h.isBackgroundPushRecordValid(pushedAt, now.UnixMilli()) {
			h.bgPushed[taskID] = pushedAt
		}
	}
}

func (h *Handler) isBackgroundPushRecordValid(pushedAt, now int64) bool {
	age := now - pushedAt
	return age >= 0 && age <= backgroundDedupRetention.Milliseconds()
}

func (h *Handler) pruneBackgroundPushesLocked(now int64) {
	for taskID, pushedAt := range h.bgPushed {
		if !h.isBackgroundPushRecordValid(pushedAt, now) {
			delete(h.bgPushed, taskID)
		}
	}
}

// saveBackgroundPushesLocked 原子写入去重状态；调用方必须持有 bgPushedMu。
func (h *Handler) saveBackgroundPushesLocked() {
	if h.bgPushedPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.bgPushedPath), 0755); err != nil {
		log.Printf("[BG] Failed to create pushed-task state directory: %v", err)
		return
	}
	data, err := json.Marshal(h.bgPushed)
	if err != nil {
		log.Printf("[BG] Failed to marshal pushed-task state: %v", err)
		return
	}
	tempFile, err := os.CreateTemp(filepath.Dir(h.bgPushedPath), ".background-pushed-*")
	if err != nil {
		log.Printf("[BG] Failed to create pushed-task state file: %v", err)
		return
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		log.Printf("[BG] Failed to write pushed-task state: %v", err)
		return
	}
	if err := tempFile.Close(); err != nil {
		log.Printf("[BG] Failed to close pushed-task state: %v", err)
		return
	}
	if err := os.Rename(tempPath, h.bgPushedPath); err != nil {
		log.Printf("[BG] Failed to save pushed-task state: %v", err)
	}
}

// HandleWS 处理 WebSocket 连接
func (h *Handler) HandleWS(w http.ResponseWriter, r *http.Request) {
	// 认证
	token := r.URL.Query().Get("token")
	if !h.auth.ValidateToken(token) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	connection := &Connection{
		ID:   uuid.New().String(),
		Conn: conn,
		Send: make(chan *protocol.Message, 256),
		Type: ConnTypeUser, // 默认为用户连接
	}

	// 注册用户连接到 Hub
	h.hub.mu.Lock()
	h.hub.users[connection.ID] = connection
	h.hub.mu.Unlock()

	// 启动写协程
	go h.writePump(connection)

	// 读协程
	h.readPump(connection)
}

// readPump 读取消息
func (h *Handler) readPump(conn *Connection) {
	defer func() {
		h.hub.Unregister(conn)
		conn.Conn.Close()
	}()

	conn.Conn.SetReadLimit(24 << 20) // 支持 16 MiB 文件经 JSON/base64 回传
	conn.Conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	conn.Conn.SetPongHandler(func(string) error {
		conn.Conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		return nil
	})

	for {
		_, data, err := conn.Conn.ReadMessage()
		if err != nil {
			break
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("JSON unmarshal error: %v", err)
			continue
		}

		// 应用层解密
		decrypted, err := protocol.DecryptMessage(h.encryptionKey, &msg)
		if err != nil {
			log.Printf("Decrypt error: %v", err)
			continue
		}

		h.handleMessage(conn, decrypted)
	}
}

// writePump 写入消息
func (h *Handler) writePump(conn *Connection) {
	ticker := time.NewTicker(15 * time.Second)
	defer func() {
		ticker.Stop()
		conn.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-conn.Send:
			conn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				conn.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// 应用层加密（仅对 Client 连接加密，Web UI 保持明文）
			outMsg := msg
			if conn.Type == ConnTypeClient {
				encrypted, err := protocol.EncryptMessage(h.encryptionKey, msg)
				if err != nil {
					log.Printf("Encrypt error (fatal): %v", err)
					return
				}
				outMsg = encrypted
			}

			data, err := json.Marshal(outMsg)
			if err != nil {
				log.Printf("JSON marshal error: %v", err)
				continue
			}

			if err := conn.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			conn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleMessage 处理消息
func (h *Handler) handleMessage(conn *Connection, msg *protocol.Message) {
	switch msg.Type {
	case protocol.TypeRegister:
		h.handleRegister(conn, msg)

	case protocol.TypeList:
		h.handleList(conn)

	case protocol.TypeAttach:
		h.handleAttach(conn, msg)

	case protocol.TypeDetach:
		h.handleDetach(conn)

	case protocol.TypeInput:
		h.handleInput(conn, msg)

	case protocol.TypeResize:
		h.handleResize(conn, msg)

	case protocol.TypeKillSession:
		h.handleKillSession(conn)

	case protocol.TypeOutput:
		h.handleOutput(conn, msg)

	case protocol.TypeChatInput, protocol.TypeNewSession:
		// 聊天输入/新会话：从 user 转发到 client
		h.handleChatUserToClient(conn, msg)

	case protocol.TypeChatMessage, protocol.TypeChatReady, protocol.TypeChatError, protocol.TypeChatAck, protocol.TypeSessionInfo, protocol.TypePermissionRequest:
		// 聊天消息/权限请求：从 client 转发到 user
		h.handleChatClientToUser(conn, msg)

	case protocol.TypePermissionResponse:
		// 权限审批结果：从 user 转发到 client
		log.Printf("[PERM] Server received permission_response from user %s", conn.ID)
		h.handleChatUserToClient(conn, msg)

	case protocol.TypePong:
		// 心跳响应，重置读超时
		conn.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	case protocol.TypeBackgroundResult:
		h.handleBackgroundResult(conn, msg)

	case protocol.TypeFileResponse:
		if conn.Type != ConnTypeClient {
			return
		}
		var payload protocol.FileResponsePayload
		if err := msg.ParsePayload(&payload); err != nil {
			return
		}
		h.shareBroker.resolve(payload)
	}
}

// handleRegister 处理客户端注册
func (h *Handler) handleRegister(conn *Connection, msg *protocol.Message) {
	var payload protocol.RegisterPayload
	if err := msg.ParsePayload(&payload); err != nil {
		conn.Send <- &protocol.Message{
			Type: protocol.TypeError,
			Payload: mustMarshal(protocol.ErrorPayload{
				Code:    400,
				Message: "invalid register payload",
			}),
		}
		return
	}

	// 清理初始注册时写入 users map 的旧条目（conn.ID 原值是 UUID）
	oldID := conn.ID
	if oldID != payload.ID {
		h.hub.mu.Lock()
		delete(h.hub.users, oldID)
		h.hub.mu.Unlock()
	}

	// 更新连接信息
	conn.ID = payload.ID
	conn.Type = ConnTypeClient
	conn.ClientID = payload.ID
	conn.ClientName = payload.Name
	conn.Description = payload.Description
	conn.ClawbotID = payload.ClawbotID
	conn.PID = payload.PID
	conn.Path = payload.Path

	// 注册到 Hub
	h.hub.RegisterClient(conn)

	// 发送确认
	conn.Send <- &protocol.Message{
		Type: protocol.TypeAck,
		Payload: mustMarshal(protocol.StatusPayload{
			ClientID: payload.ID,
			Online:   true,
			Message:  "registered",
		}),
	}

	log.Printf("Client registered: %s (%s)", payload.ID, payload.Name)
}

// handleList 处理列表请求
func (h *Handler) handleList(conn *Connection) {
	clients := h.hub.ListClients()
	conn.Send <- &protocol.Message{
		Type: protocol.TypeList,
		Payload: mustMarshal(protocol.ListPayload{
			Clients: clients,
		}),
	}
}

// handleAttach 处理附加请求
func (h *Handler) handleAttach(conn *Connection, msg *protocol.Message) {
	var payload protocol.AttachPayload
	if err := msg.ParsePayload(&payload); err != nil {
		conn.Send <- &protocol.Message{
			Type: protocol.TypeError,
			Payload: mustMarshal(protocol.ErrorPayload{
				Code:    400,
				Message: "invalid attach payload",
			}),
		}
		return
	}

	// 设置连接类型
	conn.Type = ConnTypeUser

	// 如果用户之前附加在其他客户端，先通知旧客户端 detach
	if oldClientID, ok := h.hub.GetAttachedClientID(conn.ID); ok && oldClientID != payload.ClientID {
		if oldClient, ok := h.hub.GetClient(oldClientID); ok {
			oldClient.Send <- &protocol.Message{
				Type: protocol.TypeDetach,
				From: conn.ID,
			}
			log.Printf("User %s switched from client %s to %s, notified old client", conn.ID, oldClientID, payload.ClientID)
		}
	}

	// 附加到客户端
	if !h.hub.AttachUser(conn.ID, payload.ClientID) {
		conn.Send <- &protocol.Message{
			Type: protocol.TypeError,
			Payload: mustMarshal(protocol.ErrorPayload{
				Code:    404,
				Message: "client not found",
			}),
		}
		return
	}

	// 通知客户端有用户连接
	client, _ := h.hub.GetClient(payload.ClientID)
	if client != nil {
		client.Send <- &protocol.Message{
			Type: protocol.TypeAttach,
			From: conn.ID,
		}
	}

	// 通知用户连接成功
	conn.Send <- &protocol.Message{
		Type: protocol.TypeAttached,
		Payload: mustMarshal(protocol.StatusPayload{
			ClientID: payload.ClientID,
			Online:   true,
			Message:  "attached",
		}),
	}

	log.Printf("User %s attached to client %s", conn.ID, payload.ClientID)
}

// handleDetach 处理分离请求
func (h *Handler) handleDetach(conn *Connection) {
	client, _ := h.hub.GetAttachedClient(conn.ID)
	if client != nil {
		client.Send <- &protocol.Message{
			Type: protocol.TypeDetach,
			From: conn.ID,
		}
	}

	h.hub.DetachUser(conn.ID)

	conn.Send <- &protocol.Message{
		Type: protocol.TypeDetached,
		Payload: mustMarshal(protocol.StatusPayload{
			Online:  false,
			Message: "detached",
		}),
	}
}

// handleInput 处理输入
func (h *Handler) handleInput(conn *Connection, msg *protocol.Message) {
	client, ok := h.hub.GetAttachedClient(conn.ID)
	if !ok {
		return
	}

	msg.From = conn.ID
	client.Send <- msg
}

// handleResize 处理终端尺寸变化
func (h *Handler) handleResize(conn *Connection, msg *protocol.Message) {
	client, ok := h.hub.GetAttachedClient(conn.ID)
	if !ok {
		return
	}

	msg.From = conn.ID
	client.Send <- msg
}

// handleOutput 处理输出（从客户端转发给用户）
func (h *Handler) handleOutput(conn *Connection, msg *protocol.Message) {
	// 找到附加到此客户端的用户
	h.hub.mu.RLock()
	var targetUser *Connection
	for userID, clientID := range h.hub.attachMap {
		if clientID == conn.ID {
			if user, ok := h.hub.users[userID]; ok {
				targetUser = user
				break
			}
		}
	}
	h.hub.mu.RUnlock()

	if targetUser != nil {
		targetUser.Send <- msg
	}
}

// handleBackgroundResult 处理后台任务结果（Client→Server）
func (h *Handler) handleBackgroundResult(conn *Connection, msg *protocol.Message) {
	var payload protocol.BackgroundResultPayload
	if err := msg.ParsePayload(&payload); err != nil {
		log.Printf("[BG] Failed to parse background_result: %v", err)
		return
	}
	ack := func() {
		ackMsg, _ := protocol.NewMessage(protocol.TypeBackgroundAck, protocol.BackgroundAckPayload{TaskID: payload.TaskID})
		select {
		case conn.Send <- ackMsg:
		default:
			log.Printf("[BG] BackgroundAck queue full: taskID=%s", payload.TaskID)
		}
	}

	log.Printf("[BG] Received background result: taskID=%s wechatID=%s isError=%v textLen=%d",
		payload.TaskID, payload.WechatID, payload.IsError, len(payload.FullText))

	if h.wechatMgr == nil {
		log.Printf("[BG] No WeChatManager, cannot push result")
		return
	}

	// P0-b: 时效 + 去重闸门（防断线堆积的 BackgroundResult 重连后洪泛）
	now := time.Now().UnixMilli()
	if payload.CreatedAt <= 0 {
		log.Printf("[BG] Rejected background result without creation time: taskID=%s", payload.TaskID)
		return
	}
	if payload.CreatedAt > now+backgroundMaxClockSkew.Milliseconds() {
		log.Printf("[BG] Rejected background result from the future: taskID=%s createdAt=%d", payload.TaskID, payload.CreatedAt)
		return
	}
	if now-payload.CreatedAt > backgroundResultTTL.Milliseconds() {
		log.Printf("[BG] Rejected stale background result: taskID=%s age=%dms (>5min)", payload.TaskID, now-payload.CreatedAt)
		return
	}
	h.bgPushedMu.Lock()
	h.pruneBackgroundPushesLocked(now)
	if _, ok := h.bgPushed[payload.TaskID]; ok {
		h.bgPushedMu.Unlock()
		log.Printf("[BG] Rejected duplicate background result: taskID=%s", payload.TaskID)
		ack()
		return
	}
	h.bgPushed[payload.TaskID] = now
	h.bgPushedMu.Unlock()

	// 推送结果到微信用户
	text := payload.FullText
	if payload.IsError {
		text = fmt.Sprintf("❌ 后台任务失败: %s", payload.ErrorMsg)
	}
	if text == "" {
		text = "（后台任务完成，无文本输出）"
	}

	_, err := h.wechatMgr.PushMessage(payload.WechatID, text)
	if err != nil {
		h.bgPushedMu.Lock()
		delete(h.bgPushed, payload.TaskID)
		h.bgPushedMu.Unlock()
		log.Printf("[BG] Failed to push background result: taskID=%s err=%v", payload.TaskID, err)
	} else {
		h.bgPushedMu.Lock()
		h.saveBackgroundPushesLocked()
		h.bgPushedMu.Unlock()
		log.Printf("[BG] Background result pushed: taskID=%s wechatID=%s", payload.TaskID, payload.WechatID)
		ack()
	}
}

// handleKillSession 处理销毁会话请求
func (h *Handler) handleKillSession(conn *Connection) {
	client, ok := h.hub.GetAttachedClient(conn.ID)
	if !ok {
		return
	}

	// 转发给客户端执行销毁
	client.Send <- &protocol.Message{
		Type: protocol.TypeKillSession,
		From: conn.ID,
	}

	// 断开用户连接
	h.hub.DetachUser(conn.ID)
	conn.Send <- &protocol.Message{
		Type: protocol.TypeDetached,
		Payload: mustMarshal(protocol.StatusPayload{
			Online:  false,
			Message: "session killed",
		}),
	}
}

// handleChatUserToClient 转发聊天消息从用户到客户端
func (h *Handler) handleChatUserToClient(conn *Connection, msg *protocol.Message) {
	client, ok := h.hub.GetAttachedClient(conn.ID)
	if !ok {
		log.Printf("[PERM] handleChatUserToClient: no attached client for user %s", conn.ID)
		return
	}
	msg.From = conn.ID
	client.Send <- msg
	log.Printf("[PERM] Server forwarded message to client %s", client.ID)
}

// handleChatClientToUser 转发聊天消息从客户端到用户
func (h *Handler) handleChatClientToUser(conn *Connection, msg *protocol.Message) {
	h.hub.mu.RLock()
	var targetUser *Connection
	for userID, clientID := range h.hub.attachMap {
		if clientID == conn.ID {
			if user, ok := h.hub.users[userID]; ok {
				targetUser = user
				break
			}
		}
	}
	h.hub.mu.RUnlock()

	if targetUser != nil {
		targetUser.Send <- msg
	}
}
