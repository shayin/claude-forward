package server

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
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
	ID                 string
	Type               ConnType
	Conn               *websocket.Conn
	ClientID           string   // 如果是用户连接，记录其附加的客户端ID
	ClientName         string   // 客户端名称（注册时设置）
	Description        string   // 客户端描述（注册时设置）
	ClawbotID          string   // 电脑级别 ID（用于微信路由）
	PID                int      // 客户端进程 ID
	Path               string   // 客户端工作目录
	Features           []string // 客户端声明的可选能力
	Send               chan *protocol.Message
	mu                 sync.Mutex
	lastCmuxDiagnostic time.Time // 仅用于该连接的诊断日志限速
}

func (h *Hub) HasCmuxAttachment(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cmuxAttachMap[userID] != ""
}

// Hub 管理所有连接
type Hub struct {
	clients                 map[string]*Connection                   // 已注册的本地客户端
	users                   map[string]*Connection                   // 用户连接
	attachMap               map[string]string                        // 用户ID -> 客户端ID 映射
	cmuxAttachMap           map[string]string                        // cmux 独立连接，不触发旧聊天 detach/tmux 逻辑
	cmuxPending             map[string]map[string]cmuxPendingRequest // 用户 -> 请求 ID -> 当前 Client
	cmuxEventGaps           map[string]bool                          // 用户发送队列满后，下个可写时通知丢帧
	cmuxLeases              map[string]cmuxInputLease                // Client + surface -> 当前 Web 输入设备
	cmuxLeaseRevisions      map[string]uint64                        // Client + surface -> 所有权单调代次
	cmuxViewportGenerations map[string]int                           // Client + surface + Web 连接 -> 单调视口代次
	register                chan *Connection
	unregister              chan *Connection
	mu                      sync.RWMutex
}

type cmuxPendingRequest struct {
	client    *Connection
	expiresAt time.Time
	leaseKey  string // 不可撤销的在途管理写操作，接管须等回执或超时。
}

type cmuxInputLease struct {
	client     *Connection
	ownerID    string
	expiresAt  time.Time
	generation int
	revision   uint64
}

const cmuxLeaseDuration = 45 * time.Second

// NewHub 创建新的 Hub
func NewHub() *Hub {
	return &Hub{
		clients:                 make(map[string]*Connection),
		users:                   make(map[string]*Connection),
		attachMap:               make(map[string]string),
		cmuxAttachMap:           make(map[string]string),
		cmuxPending:             make(map[string]map[string]cmuxPendingRequest),
		cmuxEventGaps:           make(map[string]bool),
		cmuxLeases:              make(map[string]cmuxInputLease),
		cmuxLeaseRevisions:      make(map[string]uint64),
		cmuxViewportGenerations: make(map[string]int),
		register:                make(chan *Connection, 256),
		unregister:              make(chan *Connection, 256),
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
					for userID, clientID := range h.cmuxAttachMap {
						if clientID == conn.ID {
							if user, ok := h.users[userID]; ok {
								select {
								case user.Send <- &protocol.Message{Type: protocol.TypeDetached,
									Payload: mustMarshal(protocol.StatusPayload{ClientID: conn.ID, Online: false, Message: "client disconnected"})}:
								default:
								}
							}
							delete(h.cmuxAttachMap, userID)
							delete(h.cmuxPending, userID)
							delete(h.cmuxEventGaps, userID)
						}
					}
					for key, lease := range h.cmuxLeases {
						if lease.client == conn {
							delete(h.cmuxLeases, key)
						}
					}
					for key := range h.cmuxViewportGenerations {
						if len(key) > len(conn.ID) && key[:len(conn.ID)+1] == conn.ID+"\x00" {
							delete(h.cmuxViewportGenerations, key)
						}
					}
					for key := range h.cmuxLeaseRevisions {
						if len(key) > len(conn.ID) && key[:len(conn.ID)+1] == conn.ID+"\x00" {
							delete(h.cmuxLeaseRevisions, key)
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
					delete(h.cmuxAttachMap, conn.ID)
					delete(h.cmuxPending, conn.ID)
					delete(h.cmuxEventGaps, conn.ID)
					h.releaseCmuxLeasesLocked(conn.ID)
					for key := range h.cmuxViewportGenerations {
						if len(key) >= len(conn.ID)+1 &&
							key[len(key)-len(conn.ID)-1:] == "\x00"+conn.ID {
							delete(h.cmuxViewportGenerations, key)
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

// RegisterBotUser 注册虚拟 Bot 用户（同步，直接操作 map）
func (h *Hub) RegisterBotUser(conn *Connection) {
	h.mu.Lock()
	h.users[conn.ID] = conn
	h.mu.Unlock()
}

// CleanupBotUser 清理虚拟 Bot 用户
func (h *Hub) CleanupBotUser(conn *Connection) {
	h.mu.Lock()
	// 指针比较防止误删
	if existing, ok := h.users[conn.ID]; ok && existing == conn {
		delete(h.users, conn.ID)
		delete(h.attachMap, conn.ID)
	}
	h.mu.Unlock()
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
			ClawbotID:   conn.ClawbotID,
			PID:         conn.PID,
			Path:        conn.Path,
			Features:    conn.Features,
			Online:      true,
		})
	}
	return clients
}

// AttachCmuxUser 独立绑定终端连接，不改变聊天 attach 状态。
func (h *Hub) AttachCmuxUser(userID, clientID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	client, ok := h.clients[clientID]
	if !ok || !clientHasFeature(client, "cmux") {
		return false
	}
	if h.cmuxAttachMap[userID] != clientID {
		h.releaseCmuxLeasesLocked(userID)
	}
	h.cmuxAttachMap[userID] = clientID
	delete(h.cmuxPending, userID)
	delete(h.cmuxEventGaps, userID)
	return true
}

func cmuxLeaseKey(clientID, surfaceID string) string {
	return clientID + "\x00" + surfaceID
}

func cmuxViewportKey(leaseKey, userID string) string {
	return leaseKey + "\x00" + userID
}

// 先把新所有者发给 Client，Client 再按代次拒绝旧设备积压的命令。
// 此操作由 Hub 锁串行，保证请求与所有权变更在同一个出站通道中有序。
func (h *Hub) announceCmuxOwnerLocked(key string, client *Connection, ownerID string) (uint64, bool) {
	if h.clients[client.ID] != client {
		return 0, false
	}
	revision := h.cmuxLeaseRevisions[key] + 1
	payload := protocol.CmuxOwnershipPayload{
		SurfaceID: key[len(client.ID)+1:], ViewerID: ownerID, Revision: revision,
	}
	message, _ := protocol.NewMessage(protocol.TypeCmuxOwnership, payload)
	select {
	case client.Send <- message:
		h.cmuxLeaseRevisions[key] = revision
		return revision, true
	default:
		return 0, false
	}
}

// 释放时即使 Client 队列暂满，也必须保留新代次，稍后再发撤销标记。
func (h *Hub) revokeCmuxOwnerLocked(key string, client *Connection) {
	if _, ok := h.announceCmuxOwnerLocked(key, client, ""); ok {
		return
	}
	if h.clients[client.ID] != client {
		return
	}
	revision := h.cmuxLeaseRevisions[key] + 1
	h.cmuxLeaseRevisions[key] = revision
	message, _ := protocol.NewMessage(protocol.TypeCmuxOwnership, protocol.CmuxOwnershipPayload{
		SurfaceID: key[len(client.ID)+1:], Revision: revision,
	})
	go h.retryCmuxOwnership(client, key, revision, message)
}

func (h *Hub) retryCmuxOwnership(client *Connection, key string, revision uint64, message *protocol.Message) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			h.mu.Lock()
			if h.clients[client.ID] != client || h.cmuxLeaseRevisions[key] != revision {
				h.mu.Unlock()
				return
			}
			select {
			case client.Send <- message:
				h.mu.Unlock()
				return
			default:
			}
			h.mu.Unlock()
		case <-timeout.C:
			log.Printf("[cmux] ownership revocation could not reach client before timeout")
			return
		}
	}
}

// releaseCmuxLeasesLocked 在用户离线/换电脑时释放，防止占住输入权。
func (h *Hub) releaseCmuxLeasesLocked(userID string) {
	for key, lease := range h.cmuxLeases {
		if lease.ownerID == userID {
			h.revokeCmuxOwnerLocked(key, lease.client)
			h.clearCmuxViewportLocked(key, lease)
			delete(h.cmuxLeases, key)
			h.broadcastCmuxLeaseLocked(lease.client.ID, key[len(lease.client.ID)+1:], userID)
		}
	}
}

// 终端视口属于 Web 连接，释放租约时恢复 cmux 原生尺寸协商。
func (h *Hub) clearCmuxViewportLocked(key string, lease cmuxInputLease) {
	if lease.generation == 0 || h.clients[lease.client.ID] != lease.client {
		return
	}
	surfaceID := key[len(lease.client.ID)+1:]
	request := protocol.CmuxRequestPayload{
		RequestID: "cf-clear-" + uuid.NewString(), Action: protocol.CmuxViewportClear,
		SurfaceID: surfaceID, ViewerID: lease.ownerID,
	}
	keyForViewer := cmuxViewportKey(key, lease.ownerID)
	request.Generation = h.cmuxViewportGenerations[keyForViewer] + 1
	message, _ := protocol.NewMessage(protocol.TypeCmuxRequest, request)
	message.To = lease.client.ID // 无用户回包；不能阻塞 Hub 的断线清理。
	select {
	case lease.client.Send <- message:
		h.cmuxViewportGenerations[keyForViewer] = request.Generation
	default:
		// Server 队列临时满时不能直接丢弃 clear，否则 cmux 会一直保留旧 Web
		// 参与者的尺寸。离开 Hub 锁后有界重试，并在重试前检查是否已有新一代上报。
		go h.retryCmuxViewportClear(lease.client, key, lease.ownerID, request.Generation, message)
	}
}

func (h *Hub) retryCmuxViewportClear(client *Connection, key, ownerID string,
	generation int, message *protocol.Message) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	viewerKey := cmuxViewportKey(key, ownerID)
	for {
		select {
		case <-ticker.C:
			h.mu.Lock()
			if h.clients[client.ID] != client ||
				h.cmuxViewportGenerations[viewerKey] >= generation {
				h.mu.Unlock()
				return
			}
			select {
			case client.Send <- message:
				if h.users[ownerID] != nil {
					h.cmuxViewportGenerations[viewerKey] = generation
				}
				h.mu.Unlock()
				return
			default:
			}
			h.mu.Unlock()
		case <-deadline.C:
			log.Printf("[cmux] viewport clear could not reach client before timeout")
			return
		}
	}
}

func (h *Hub) broadcastCmuxLeaseLocked(clientID, surfaceID, exceptUserID string) {
	key := cmuxLeaseKey(clientID, surfaceID)
	lease, occupied := h.cmuxLeases[key]
	occupied = occupied && time.Now().Before(lease.expiresAt)
	for userID, attachedClient := range h.cmuxAttachMap {
		if userID == exceptUserID || attachedClient != clientID {
			continue
		}
		user := h.users[userID]
		if user == nil || user.Type != ConnTypeUser {
			continue
		}
		payload := protocol.CmuxClaimedPayload{
			ClientID: clientID, SurfaceID: surfaceID, Owned: occupied && lease.ownerID == userID,
			Occupied: occupied,
		}
		select {
		case user.Send <- &protocol.Message{Type: protocol.TypeCmuxClaimed, Payload: mustMarshal(payload)}:
		default:
		}
	}
}

// ClaimCmuxInput 普通 claim 不抢占；用户明确选择“接管”才强制转移。
func (h *Hub) ClaimCmuxInput(userID, surfaceID string, force bool) (protocol.CmuxClaimedPayload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	clientID := h.cmuxAttachMap[userID]
	client := h.clients[clientID]
	if client == nil || !clientHasFeature(client, "cmux") || h.users[userID] == nil {
		return protocol.CmuxClaimedPayload{}, false
	}
	now := time.Now()
	for key, lease := range h.cmuxLeases {
		if now.After(lease.expiresAt) || h.clients[lease.client.ID] != lease.client {
			h.revokeCmuxOwnerLocked(key, lease.client)
			h.clearCmuxViewportLocked(key, lease)
			delete(h.cmuxLeases, key)
		}
	}
	key := cmuxLeaseKey(clientID, surfaceID)
	lease, occupied := h.cmuxLeases[key]
	if occupied && lease.ownerID != userID && !force {
		return protocol.CmuxClaimedPayload{ClientID: clientID, SurfaceID: surfaceID, Occupied: true}, true
	}
	if occupied && lease.ownerID != userID && force {
		for _, pending := range h.cmuxPending[lease.ownerID] {
			if pending.client == client && pending.leaseKey == key && now.Before(pending.expiresAt) {
				return protocol.CmuxClaimedPayload{ClientID: clientID, SurfaceID: surfaceID, Occupied: true}, true
			}
		}
	}
	if !occupied && len(h.cmuxLeases) >= 256 {
		return protocol.CmuxClaimedPayload{}, false
	}
	changed := !occupied || lease.ownerID != userID
	revision := lease.revision
	if changed {
		var announced bool
		revision, announced = h.announceCmuxOwnerLocked(key, client, userID)
		if !announced {
			return protocol.CmuxClaimedPayload{}, false
		}
	}
	if occupied && lease.ownerID != userID {
		h.clearCmuxViewportLocked(key, lease)
	}
	h.cmuxLeases[key] = cmuxInputLease{
		client: client, ownerID: userID, expiresAt: now.Add(cmuxLeaseDuration),
		generation: h.cmuxViewportGenerations[cmuxViewportKey(key, userID)],
		revision:   revision,
	}
	if changed {
		h.broadcastCmuxLeaseLocked(clientID, surfaceID, userID)
	}
	return protocol.CmuxClaimedPayload{ClientID: clientID, SurfaceID: surfaceID, Owned: true, Occupied: true}, true
}

func (h *Hub) ReleaseCmuxInput(userID, surfaceID string) (protocol.CmuxClaimedPayload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	clientID := h.cmuxAttachMap[userID]
	if clientID == "" || h.clients[clientID] == nil {
		return protocol.CmuxClaimedPayload{}, false
	}
	key := cmuxLeaseKey(clientID, surfaceID)
	if lease, ok := h.cmuxLeases[key]; ok && lease.ownerID == userID {
		h.revokeCmuxOwnerLocked(key, lease.client)
		h.clearCmuxViewportLocked(key, lease)
		delete(h.cmuxLeases, key)
		h.broadcastCmuxLeaseLocked(clientID, surfaceID, userID)
	}
	lease, occupied := h.cmuxLeases[key]
	occupied = occupied && time.Now().Before(lease.expiresAt)
	return protocol.CmuxClaimedPayload{
		ClientID: clientID, SurfaceID: surfaceID, Owned: false, Occupied: occupied && lease.ownerID != userID,
	}, true
}

// ForwardCmuxRequest 持锁验证并非阻塞投递，避免离线并发关闭 Send 时产生 panic。
func (h *Hub) ForwardCmuxRequest(userID string, request protocol.CmuxRequestPayload, msg *protocol.Message) bool {
	if len(msg.Payload) > protocol.MaxCmuxRequestPayloadBytes {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	clientID := h.cmuxAttachMap[userID]
	client := h.clients[clientID]
	if client == nil || !clientHasFeature(client, "cmux") {
		return false
	}
	now := time.Now()
	var inputLeaseKey string
	var existingLease cmuxInputLease
	var inputOccupied bool
	switch request.Action {
	case protocol.CmuxWorkspaceRename, protocol.CmuxWorkspaceCreate,
		protocol.CmuxWorkspaceClose, protocol.CmuxWorkspaceReorder,
		protocol.CmuxGroupCreate, protocol.CmuxGroupRename,
		protocol.CmuxGroupAdd, protocol.CmuxGroupRemove,
		protocol.CmuxTerminalCreate, protocol.CmuxNotificationRead,
		protocol.CmuxBrowserNavigate, protocol.CmuxBrowserClick, protocol.CmuxBrowserType,
		protocol.CmuxBrowserKey, protocol.CmuxBrowserScroll, protocol.CmuxBrowserNav,
		protocol.CmuxBrowserScreenshot:
		inputLeaseKey = cmuxLeaseKey(clientID, protocol.CmuxAdminSurfaceID)
		existingLease, inputOccupied = h.cmuxLeases[inputLeaseKey]
		if !inputOccupied || existingLease.ownerID != userID ||
			existingLease.client != client || !now.Before(existingLease.expiresAt) {
			return false
		}
	case protocol.CmuxText, protocol.CmuxKey, protocol.CmuxPaste, protocol.CmuxScroll,
		protocol.CmuxMouse, protocol.CmuxTerminalClose, protocol.CmuxViewportReport,
		protocol.CmuxViewportClear:
		inputLeaseKey = cmuxLeaseKey(clientID, request.SurfaceID)
		existingLease, inputOccupied = h.cmuxLeases[inputLeaseKey]
		// 只允许明确 claim 的当前所有者输入；Web 不能靠发送一个写请求
		// 悄悄占用无人持有或刚过期的终端。
		if !inputOccupied || existingLease.ownerID != userID ||
			existingLease.client != client || !now.Before(existingLease.expiresAt) {
			return false
		}
	case protocol.CmuxUploadBegin, protocol.CmuxUploadChunk, protocol.CmuxUploadFinish:
		surface := request.SurfaceID
		if request.UploadKind == "file" {
			surface = protocol.CmuxAdminSurfaceID
		}
		inputLeaseKey = cmuxLeaseKey(clientID, surface)
		existingLease, inputOccupied = h.cmuxLeases[inputLeaseKey]
		if !inputOccupied || existingLease.ownerID != userID ||
			existingLease.client != client || !now.Before(existingLease.expiresAt) {
			return false
		}
	}
	for id, pending := range h.cmuxPending[userID] {
		if now.After(pending.expiresAt) {
			delete(h.cmuxPending[userID], id)
		}
	}
	if len(h.cmuxPending[userID]) >= 32 {
		return false
	}
	if _, exists := h.cmuxPending[userID][request.RequestID]; exists {
		return false
	}
	if inputLeaseKey != "" {
		request.ViewerID = userID // 忽略不可信 Web viewer_id。
		request.LeaseSurfaceID = inputLeaseKey[len(clientID)+1:]
		request.LeaseRevision = existingLease.revision
		if request.Action == protocol.CmuxViewportReport ||
			request.Action == protocol.CmuxViewportClear {
			request.Generation = h.cmuxViewportGenerations[cmuxViewportKey(inputLeaseKey, userID)] + 1
		}
		msg.Payload = mustMarshal(request)
	} else if request.Action == protocol.CmuxUploadBegin ||
		request.Action == protocol.CmuxUploadChunk ||
		request.Action == protocol.CmuxUploadFinish ||
		request.Action == protocol.CmuxUploadAbort {
		request.ViewerID = userID
		msg.Payload = mustMarshal(request)
	}
	msg.From, msg.To = userID, clientID
	select {
	case client.Send <- msg:
		if inputLeaseKey != "" {
			existingLease.expiresAt = now.Add(cmuxLeaseDuration)
			if request.Action == protocol.CmuxViewportReport ||
				request.Action == protocol.CmuxViewportClear {
				existingLease.generation = request.Generation
				h.cmuxViewportGenerations[cmuxViewportKey(inputLeaseKey, userID)] = request.Generation
			}
			h.cmuxLeases[inputLeaseKey] = existingLease
		}
		if h.cmuxPending[userID] == nil {
			h.cmuxPending[userID] = make(map[string]cmuxPendingRequest)
		}
		pendingFor := 25 * time.Second
		if protocol.IsCmuxBrowserAction(request.Action) {
			pendingFor = 45 * time.Second
		}
		fenceKey := ""
		if inputLeaseKey != "" && cmuxNeedsHandoffFence(request) {
			fenceKey = inputLeaseKey
		}
		h.cmuxPending[userID][request.RequestID] = cmuxPendingRequest{
			client: client, expiresAt: now.Add(pendingFor), leaseKey: fenceKey,
		}
		return true
	default:
		return false
	}
}

func cmuxNeedsHandoffFence(request protocol.CmuxRequestPayload) bool {
	switch request.Action {
	case protocol.CmuxWorkspaceCreate, protocol.CmuxWorkspaceRename,
		protocol.CmuxWorkspaceClose, protocol.CmuxWorkspaceReorder,
		protocol.CmuxGroupCreate, protocol.CmuxGroupRename,
		protocol.CmuxGroupAdd, protocol.CmuxGroupRemove,
		protocol.CmuxTerminalCreate, protocol.CmuxTerminalClose,
		protocol.CmuxBrowserNavigate, protocol.CmuxBrowserClick,
		protocol.CmuxBrowserType, protocol.CmuxBrowserKey,
		protocol.CmuxBrowserScroll, protocol.CmuxBrowserNav,
		protocol.CmuxNotificationRead:
		return true
	case protocol.CmuxUploadBegin, protocol.CmuxUploadChunk, protocol.CmuxUploadFinish:
		return request.UploadKind == "file"
	}
	return false
}

// DeliverCmuxResponse 要求同一条当前 Client 连接，且用户仍绑定到这台电脑。
func (h *Hub) DeliverCmuxResponse(client *Connection, msg *protocol.Message) bool {
	var response protocol.CmuxResponsePayload
	if msg.ParsePayload(&response) != nil || response.RequestID == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[client.ID] != client || h.cmuxAttachMap[msg.To] != client.ID {
		return false
	}
	request, ok := h.cmuxPending[msg.To][response.RequestID]
	if !ok || request.client != client || time.Now().After(request.expiresAt) {
		return false
	}
	delete(h.cmuxPending[msg.To], response.RequestID)
	user := h.users[msg.To]
	if user == nil || user.Type != ConnTypeUser {
		return false
	}
	select {
	case user.Send <- msg:
		return true
	default:
		return false
	}
}

// BroadcastCmuxEvent 只向当前绑定到这一个 Client 的 Web 连接发送状态元数据。
// 队列满时记 gap，下一次有空间先发失效信号；不能谎称程序事件可由列表轮询补全。
func (h *Hub) BroadcastCmuxEvent(client *Connection, msg *protocol.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[client.ID] != client {
		return
	}
	for userID, clientID := range h.cmuxAttachMap {
		if clientID != client.ID {
			continue
		}
		if user := h.users[userID]; user != nil && user.Type == ConnTypeUser {
			if h.cmuxEventGaps[userID] {
				gap, _ := protocol.NewMessage(protocol.TypeCmuxEvent,
					protocol.CmuxEventPayload{Resync: true, Gap: true})
				gap.From = client.ID
				select {
				case user.Send <- gap:
					delete(h.cmuxEventGaps, userID)
				default:
					continue
				}
			}
			select {
			case user.Send <- msg:
			default:
				h.cmuxEventGaps[userID] = true
			}
		}
	}
}

func clientHasFeature(client *Connection, feature string) bool {
	for _, current := range client.Features {
		if current == feature {
			return true
		}
	}
	return false
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

// FindClientByClawbotID 按 ClawbotID 查找第一个在线的 Client
func (h *Hub) FindClientByClawbotID(clawbotID string) (*Connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.clients {
		if conn.ClawbotID == clawbotID {
			return conn, true
		}
	}
	return nil, false
}

// ListClientsByClawbotID 按 ClawbotID 列出所有 Client
func (h *Hub) ListClientsByClawbotID(clawbotID string) []protocol.ClientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var clients []protocol.ClientInfo
	for _, conn := range h.clients {
		if conn.ClawbotID == clawbotID {
			clients = append(clients, protocol.ClientInfo{
				ID:          conn.ID,
				Name:        conn.ClientName,
				Description: conn.Description,
				ClawbotID:   conn.ClawbotID,
				PID:         conn.PID,
				Path:        conn.Path,
				Online:      true,
			})
		}
	}
	return clients
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
