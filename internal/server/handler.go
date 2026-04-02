package server

import (
	"encoding/json"
	"log"
	"net/http"
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

// Handler WebSocket 处理器
type Handler struct {
	hub  *Hub
	auth *Auth
}

// NewHandler 创建处理器
func NewHandler(hub *Hub, auth *Auth) *Handler {
	return &Handler{
		hub:  hub,
		auth: auth,
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

	conn.Conn.SetReadLimit(512 * 1024) // 512KB
	conn.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.Conn.SetPongHandler(func(string) error {
		conn.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
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

		h.handleMessage(conn, &msg)
	}
}

// writePump 写入消息
func (h *Handler) writePump(conn *Connection) {
	ticker := time.NewTicker(30 * time.Second)
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

			data, err := json.Marshal(msg)
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

	case protocol.TypePong:
		// 心跳响应，重置读超时
		conn.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
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

	// 更新连接信息
	conn.ID = payload.ID
	conn.Type = ConnTypeClient
	conn.ClientID = payload.ID
	conn.ClientName = payload.Name
	conn.Description = payload.Description
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
