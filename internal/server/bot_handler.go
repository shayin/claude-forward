package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
)

// BotHandler 处理 Bot API 请求
type BotHandler struct {
	hub  *Hub
	auth *Auth
}

// NewBotHandler 创建 Bot API 处理器
func NewBotHandler(hub *Hub, auth *Auth) *BotHandler {
	return &BotHandler{hub: hub, auth: auth}
}

// botChatRequest 聊天请求
type botChatRequest struct {
	ClawbotID string `json:"clawbot_id"` // 电脑级别 ID（优先）
	ClientID  string `json:"client_id"`  // 具体客户端 ID（clawbot_id 为空时使用）
	Text      string `json:"text"`
}

// HandleChat 处理聊天请求（SSE 流式回复）
func (bh *BotHandler) HandleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !bh.authenticate(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req botChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	// 解析目标 Client：优先 clawbot_id，其次 client_id
	clientID := req.ClientID
	if clientID == "" && req.ClawbotID != "" {
		// 按 clawbot_id 找第一个在线的 Client
		conn, ok := bh.hub.FindClientByClawbotID(req.ClawbotID)
		if !ok {
			http.Error(w, fmt.Sprintf("no online client for clawbot_id %q", req.ClawbotID), http.StatusNotFound)
			return
		}
		clientID = conn.ID
	}

	if clientID == "" {
		http.Error(w, "clawbot_id or client_id is required", http.StatusBadRequest)
		return
	}

	client, ok := bh.hub.GetClient(clientID)
	if !ok {
		http.Error(w, fmt.Sprintf("client %s not found or offline", clientID), http.StatusNotFound)
		return
	}

	// 创建虚拟 Bot 连接
	botConn := &Connection{
		ID:   "bot-" + uuid.New().String(),
		Type: ConnTypeUser,
		Send: make(chan *protocol.Message, 256),
	}

	bh.hub.RegisterBotUser(botConn)
	defer bh.hub.CleanupBotUser(botConn)

	if !bh.hub.AttachUser(botConn.ID, clientID) {
		http.Error(w, "failed to attach to client", http.StatusInternalServerError)
		return
	}
	defer bh.hub.DetachUser(botConn.ID)

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// 先发送 attach 通知，客户端需要知道用户 ID 才会转发 Claude 回复
	attachMsg := &protocol.Message{
		Type: protocol.TypeAttach,
		From: botConn.ID,
	}
	client.Send <- attachMsg

	// 发送 chat_input
	chatMsg, err := protocol.NewMessage(protocol.TypeChatInput, protocol.ChatInputPayload{
		Text: req.Text,
	})
	if err != nil {
		http.Error(w, "failed to create message", http.StatusInternalServerError)
		return
	}
	chatMsg.From = botConn.ID
	client.Send <- chatMsg

	log.Printf("[BOT] Sent chat_input from %s to client %s (clawbot_id=%s)", botConn.ID, clientID, req.ClawbotID)

	// SSE 流推送
	// 结束时通知客户端 detach，清理 attachedUser
	defer func() {
		client.Send <- &protocol.Message{
			Type: protocol.TypeDetach,
			From: botConn.ID,
		}
	}()

	flusher, canFlush := w.(http.Flusher)
	timeout := time.NewTimer(5 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case msg, ok := <-botConn.Send:
			if !ok {
				return
			}

			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if canFlush {
				flusher.Flush()
			}

			if msg.Type == protocol.TypeChatReady || msg.Type == protocol.TypeChatError {
				log.Printf("[BOT] Chat completed for %s (type=%s)", botConn.ID, msg.Type)
				return
			}

			timeout.Reset(5 * time.Minute)

		case <-timeout.C:
			log.Printf("[BOT] Chat timeout for %s", botConn.ID)
			errEvent, _ := json.Marshal(&protocol.Message{
				Type: protocol.TypeChatError,
				Payload: mustMarshal(protocol.ErrorPayload{
					Code:    408,
					Message: "chat timeout",
				}),
			})
			fmt.Fprintf(w, "data: %s\n\n", errEvent)
			if canFlush {
				flusher.Flush()
			}
			return

		case <-r.Context().Done():
			log.Printf("[BOT] Client disconnected for %s", botConn.ID)
			return
		}
	}
}

// HandleClients 列出可用客户端
// GET /api/bot/clients                — 列出所有
// GET /api/bot/clients?clawbot_id=xxx — 按电脑筛选
func (bh *BotHandler) HandleClients(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !bh.authenticate(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clawbotID := r.URL.Query().Get("clawbot_id")
	var clients []protocol.ClientInfo
	if clawbotID != "" {
		clients = bh.hub.ListClientsByClawbotID(clawbotID)
	} else {
		clients = bh.hub.ListClients()
	}
	json.NewEncoder(w).Encode(clients)
}

// authenticate 验证请求认证
func (bh *BotHandler) authenticate(r *http.Request) bool {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return bh.auth.ValidateToken(token)
}
