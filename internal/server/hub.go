package server

import (
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/shayin/claude-forward/internal/protocol"
)

// ConnType 连接类型
type ConnType int

const (
	ConnTypeClient ConnType = iota // 本地客户端（运行 Claude）
	ConnTypeUser                   // 用户连接（Web/CLI）
)

// Connection 表示一个 WebSocket 连接
type Connection struct {
	ID          string
	Type        ConnType
	Conn        *websocket.Conn
	ClientID    string // 如果是用户连接，记录其附加的客户端ID
	ClientName  string // 客户端名称（注册时设置）
	Description string // 客户端描述（注册时设置）
	PID         int    // 客户端进程 ID
	Path        string // 客户端工作目录
	Send        chan *protocol.Message
	mu          sync.Mutex
}

// Hub 管理所有连接
type Hub struct {
	clients    map[string]*Connection // 已注册的本地客户端
	users      map[string]*Connection // 用户连接
	attachMap  map[string]string      // 用户ID -> 客户端ID 映射
	register   chan *Connection
	unregister chan *Connection
	mu         sync.RWMutex
}

// NewHub 创建新的 Hub
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Connection),
		users:      make(map[string]*Connection),
		attachMap:  make(map[string]string),
		register:   make(chan *Connection, 256),
		unregister: make(chan *Connection, 256),
	}
}

// Run 运行 Hub
func (h *Hub) Run() {
	for {
		select {
		case conn := <-h.register:
			h.mu.Lock()
			if conn.Type == ConnTypeClient {
				h.clients[conn.ID] = conn
			} else {
				h.users[conn.ID] = conn
			}
			h.mu.Unlock()

		case conn := <-h.unregister:
			h.mu.Lock()
			if conn.Type == ConnTypeClient {
				// 指针比较：只有 map 中存储的连接就是要注销的连接时才删除
				// 防止旧连接注销时误删同 ID 的新连接
				if existing, ok := h.clients[conn.ID]; ok && existing == conn {
					delete(h.clients, conn.ID)
					// 通知所有附加到此客户端的用户
					for userID, clientID := range h.attachMap {
						if clientID == conn.ID {
							if user, ok := h.users[userID]; ok {
								user.Send <- &protocol.Message{
									Type: protocol.TypeDetached,
									Payload: mustMarshal(protocol.StatusPayload{
										ClientID: conn.ID,
										Online:   false,
										Message:  "client disconnected",
									}),
								}
							}
							delete(h.attachMap, userID)
						}
					}
				}
			} else {
				// 指针比较：防止旧连接注销时误删同 ID 的新连接
				if existing, ok := h.users[conn.ID]; ok && existing == conn {
					delete(h.users, conn.ID)
					if clientID, ok := h.attachMap[conn.ID]; ok {
						delete(h.attachMap, conn.ID)
						// 通知 Client 该用户已断开
						if client, ok := h.clients[clientID]; ok {
							client.Send <- &protocol.Message{
								Type: protocol.TypeDetach,
								From: conn.ID,
							}
						}
					}
				}
			}
			h.mu.Unlock()
			close(conn.Send)
		}
	}
}

// RegisterClient 注册本地客户端
func (h *Hub) RegisterClient(conn *Connection) {
	h.register <- conn
}

// Unregister 注销连接
func (h *Hub) Unregister(conn *Connection) {
	h.unregister <- conn
}

// GetClient 获取客户端
func (h *Hub) GetClient(id string) (*Connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.clients[id]
	return conn, ok
}

// ListClients 列出所有客户端
func (h *Hub) ListClients() []protocol.ClientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var clients []protocol.ClientInfo
	for _, conn := range h.clients {
		clients = append(clients, protocol.ClientInfo{
			ID:          conn.ID,
			Name:        conn.ClientName,
			Description: conn.Description,
			PID:         conn.PID,
			Path:        conn.Path,
			Online:      true,
		})
	}
	return clients
}

// AttachUser 将用户附加到客户端
func (h *Hub) AttachUser(userID, clientID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.clients[clientID]; !ok {
		return false
	}

	h.attachMap[userID] = clientID
	if user, ok := h.users[userID]; ok {
		user.ClientID = clientID
	}
	return true
}

// DetachUser 分离用户
func (h *Hub) DetachUser(userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.attachMap, userID)
	if user, ok := h.users[userID]; ok {
		user.ClientID = ""
	}
}

// GetAttachedClientID 获取用户附加的客户端ID（不返回连接对象）
func (h *Hub) GetAttachedClientID(userID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	clientID, ok := h.attachMap[userID]
	return clientID, ok
}

// GetAttachedClient 获取用户附加的客户端
func (h *Hub) GetAttachedClient(userID string) (*Connection, bool) {
	h.mu.RLock()
	clientID, ok := h.attachMap[userID]
	if !ok {
		h.mu.RUnlock()
		return nil, false
	}
	conn, ok := h.clients[clientID]
	h.mu.RUnlock()
	return conn, ok
}

// GetUser 获取用户连接
func (h *Hub) GetUser(id string) (*Connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.users[id]
	return conn, ok
}

func mustMarshal(v any) json.RawMessage {
	return protocol.MustMarshal(v)
}
