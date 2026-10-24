package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
)

const maxCmuxResponseBytes = 16 << 20

var errCmuxMethodUnavailable = errors.New("当前 cmux 版本不支持此操作")

// CmuxManager 只连接现有 cmux socket，不创建终端、不修改全局配置。
type CmuxManager struct {
	config              CmuxConfig
	cliPath             string
	uploadMu            sync.Mutex
	uploads             map[string]*cmuxUpload
	browserMu           sync.Mutex
	browsers            map[string]string // 工作区 → 本机 cmux 浏览器 surface
	browserAllowedPorts map[string][]int
}

func NewCmuxManager(config CmuxConfig) *CmuxManager {
	return &CmuxManager{config: config, cliPath: "cmux",
		uploads: make(map[string]*cmuxUpload), browsers: make(map[string]string),
		browserAllowedPorts: make(map[string][]int)}
}

type cmuxRPCRequest struct {
	ID     string        `json:"id"`
	Method string        `json:"method"`
	Params cmuxRPCParams `json:"params"`
}

type cmuxRPCParams struct {
	WorkspaceID        string    `json:"workspace_id,omitempty"`
	SurfaceID          string    `json:"surface_id,omitempty"`
	TerminalID         string    `json:"terminal_id,omitempty"`
	GroupID            string    `json:"group_id,omitempty"`
	Name               string    `json:"name,omitempty"`
	Title              string    `json:"title,omitempty"`
	CWD                string    `json:"cwd,omitempty"`
	ChildWorkspaceIDs  *[]string `json:"child_workspace_ids,omitempty"`
	IdempotencyKey     string    `json:"idempotency_key,omitempty"`
	Index              *int      `json:"index,omitempty"`
	DeltaLines         int       `json:"delta_lines,omitempty"`
	Column             *int      `json:"col,omitempty"`
	Row                *int      `json:"row,omitempty"`
	NotificationID     string    `json:"id,omitempty"`
	ClientID           string    `json:"client_id,omitempty"`
	ViewportColumns    int       `json:"viewport_columns,omitempty"`
	ViewportRows       int       `json:"viewport_rows,omitempty"`
	ViewportGeneration int       `json:"viewport_generation,omitempty"`
	DeviceName         string    `json:"device_name,omitempty"`
	Clear              bool      `json:"clear,omitempty"`
	Text               string    `json:"text,omitempty"`
	Key                string    `json:"key,omitempty"`
	SubmitKey          string    `json:"submit_key,omitempty"`
	Password           string    `json:"password,omitempty"`
	URL                string    `json:"url,omitempty"`
	Script             string    `json:"script,omitempty"`
	Width              int       `json:"width,omitempty"`
	Height             int       `json:"height,omitempty"`
	DY                 int       `json:"dy,omitempty"`
}

type cmuxRPCResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (m *CmuxManager) Execute(ctx context.Context, request protocol.CmuxRequestPayload) (json.RawMessage, error) {
	return m.ExecuteWithLease(ctx, request, nil)
}

func (m *CmuxManager) ExecuteWithLease(ctx context.Context, request protocol.CmuxRequestPayload,
	leaseCurrent func() bool) (json.RawMessage, error) {
	if err := protocol.ValidateCmuxRequest(request); err != nil {
		return nil, err
	}
	if request.Action == protocol.CmuxWorkspaceCreate {
		return m.createWorkspace(ctx, request.Title, request.Directory, request.Profile)
	}
	switch request.Action {
	case protocol.CmuxFileRoots, protocol.CmuxFileList, protocol.CmuxFileRead, protocol.CmuxFileRecent:
		return m.executeFileRequest(request)
	case protocol.CmuxList:
		return m.listCmuxWorkspaces(ctx)
	case protocol.CmuxUploadBegin, protocol.CmuxUploadChunk, protocol.CmuxUploadFinish,
		protocol.CmuxUploadAbort:
		return m.executeUploadRequest(request)
	case protocol.CmuxBrowserPorts, protocol.CmuxBrowserNavigate,
		protocol.CmuxBrowserScreenshot, protocol.CmuxBrowserClick,
		protocol.CmuxBrowserType, protocol.CmuxBrowserKey,
		protocol.CmuxBrowserScroll, protocol.CmuxBrowserNav:
		return m.executeBrowserRequest(ctx, request, leaseCurrent)
	}

	params := cmuxRPCParams{}
	method := ""
	switch request.Action {
	case protocol.CmuxHealth:
		method = "system.ping"
	case protocol.CmuxList:
		method = "mobile.workspace.list"
	case protocol.CmuxSnapshot:
		method, params.SurfaceID = "terminal.replay", request.SurfaceID
	case protocol.CmuxText:
		method, params.SurfaceID, params.Text = "surface.send_text", request.SurfaceID, request.Text
	case protocol.CmuxKey:
		method, params.SurfaceID, params.Key = "surface.send_key", request.SurfaceID, request.Key
	case protocol.CmuxPaste:
		method = "mobile.terminal.paste"
		params.SurfaceID, params.TerminalID, params.Text, params.SubmitKey =
			request.SurfaceID, request.SurfaceID, request.Text, "none"
	case protocol.CmuxWorkspaceRename:
		method, params.WorkspaceID, params.Title = "workspace.rename", request.WorkspaceID, request.Title
	case protocol.CmuxWorkspaceClose:
		method, params.WorkspaceID = "workspace.close", request.WorkspaceID
	case protocol.CmuxWorkspaceReorder:
		method, params.WorkspaceID, params.Index = "workspace.reorder", request.WorkspaceID, &request.Index
	case protocol.CmuxGroupCreate:
		method, params.Name, params.CWD = "workspace.group.create", request.Title, request.Directory
		children := []string{}
		params.ChildWorkspaceIDs = &children
		params.IdempotencyKey = request.RequestID
	case protocol.CmuxGroupRename:
		method, params.GroupID, params.Name = "workspace.group.rename", request.GroupID, request.Title
	case protocol.CmuxGroupAdd:
		method, params.GroupID, params.WorkspaceID = "workspace.group.add", request.GroupID, request.WorkspaceID
	case protocol.CmuxGroupRemove:
		method, params.WorkspaceID = "workspace.group.remove", request.WorkspaceID
	case protocol.CmuxTerminalCreate:
		method, params.WorkspaceID = "mobile.terminal.create", request.WorkspaceID
	case protocol.CmuxTerminalClose:
		method, params.WorkspaceID, params.SurfaceID = "surface.close", request.WorkspaceID, request.SurfaceID
	case protocol.CmuxScroll:
		method, params.WorkspaceID, params.SurfaceID, params.DeltaLines = "mobile.terminal.scroll",
			request.WorkspaceID, request.SurfaceID, request.DeltaLines
		col, row := 5, 5
		params.Column, params.Row = &col, &row
	case protocol.CmuxMouse:
		method, params.WorkspaceID, params.SurfaceID = "mobile.terminal.mouse", request.WorkspaceID, request.SurfaceID
		params.Column, params.Row = &request.Column, &request.Row
	case protocol.CmuxViewportReport:
		if request.ViewerID == "" {
			return nil, errors.New("viewport viewer missing")
		}
		method, params.SurfaceID = "terminal.viewport", request.SurfaceID
		params.ClientID = "cf:" + request.ViewerID
		params.ViewportColumns, params.ViewportRows = request.Columns, request.Rows
		params.ViewportGeneration, params.DeviceName = request.Generation, "Claude Forward Web"
	case protocol.CmuxViewportClear:
		if request.ViewerID == "" {
			return nil, errors.New("viewport viewer missing")
		}
		method, params.SurfaceID = "terminal.viewport", request.SurfaceID
		params.ClientID, params.Clear = "cf:"+request.ViewerID, true
		params.ViewportGeneration = request.Generation
	case protocol.CmuxNotifications:
		method = "notification.list"
	case protocol.CmuxNotificationRead:
		method, params.NotificationID = "notification.mark_read", request.NotificationID
	default:
		return nil, errors.New("unsupported cmux action")
	}
	// Socket 密码只在本机 RPC 中使用，绝不由网页或云端传入。
	params.Password = os.Getenv("CMUX_SOCKET_PASSWORD")
	result, err := m.rpc(ctx, method, params)
	if request.Action == protocol.CmuxScroll && err == nil {
		// 本机回放通常远短于手机到 Server 的一趟往返；把中途画面搭在滚动回执上。
		// Codex 可能异步重绘，网页仍会在滚动结束后补读一次最终快照。
		replayCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		replay, replayErr := m.rpc(replayCtx, "terminal.replay", cmuxRPCParams{
			SurfaceID: request.SurfaceID, Password: params.Password,
		})
		if replayErr == nil {
			return replay, nil
		}
		return result, nil
	}
	if !errors.Is(err, errCmuxMethodUnavailable) {
		return result, err
	}
	var fallbacks []string
	switch request.Action {
	case protocol.CmuxSnapshot:
		fallbacks = []string{"surface.read_text"}
	case protocol.CmuxPaste:
		fallbacks = []string{"terminal.paste"}
	case protocol.CmuxTerminalCreate:
		fallbacks = []string{"terminal.create", "surface.create", "workspace.add_terminal"}
	case protocol.CmuxTerminalClose:
		fallbacks = []string{"workspace.close_terminal", "workspace.remove_terminal"}
	}
	for _, fallback := range fallbacks {
		result, err = m.rpc(ctx, fallback, params)
		if !errors.Is(err, errCmuxMethodUnavailable) {
			return result, err
		}
	}
	return nil, err
}

// 新建工作区使用 cmux 官方 CLI；只传分离参数，不解释用户输入为 shell 命令。
func (m *CmuxManager) createWorkspace(ctx context.Context, name, directory, profile string) (json.RawMessage, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("workspace directory must be absolute")
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("workspace directory unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	// cmux 旧版即使设置 --id-format uuids，新建命令仍只返回 "OK workspace:N"。
	// 创建前记录 UUID，之后只认新增且名称、目录匹配的工作区，避免把短引用当 UUID。
	existing, err := m.workspaceIDs(ctx)
	if err != nil {
		return nil, err
	}
	focus := "true" // 空 shell 需由 cmux 主动启动 PTY。
	if profile != "" {
		focus = "false" // 已有启动命令时不抢占电脑前台正在使用的工作区。
	}
	args := []string{"--id-format", "uuids", "new-workspace", "--name", name,
		"--cwd", directory, "--focus", focus}
	if profile != "" {
		// 启动命令只接受协议白名单，不转发旧聊天的权限跳过参数或任意 shell 字符串。
		args = append(args, "--command", profile)
	}
	cmd := exec.CommandContext(ctx, m.cliPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// CLI 错误不能把本地环境或路径原样发往网页。
		return nil, errors.New("cmux workspace creation failed or access denied")
	}
	for _, word := range strings.Fields(string(output)) {
		candidate := strings.Trim(word, "(),")
		if _, err := uuid.Parse(candidate); err == nil {
			return protocol.MustMarshal(struct {
				WorkspaceID string `json:"workspace_id"`
			}{candidate}), nil
		}
	}
	expectedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, errors.New("cmux workspace created but its directory could not be checked; refresh workspace list")
	}
	for attempt := 0; attempt < 8; attempt++ {
		workspaces, err := m.workspaceIDs(ctx)
		if err == nil {
			id := ""
			ambiguous := false
			for candidate, workspace := range workspaces {
				if _, existed := existing[candidate]; existed || workspace.Title != name {
					continue
				}
				actualDirectory, pathErr := filepath.EvalSymlinks(workspace.Directory)
				if pathErr != nil || actualDirectory != expectedDirectory {
					continue
				}
				if id != "" {
					ambiguous = true
					break
				}
				id = candidate
			}
			if ambiguous {
				return nil, errors.New("cmux workspace created but multiple matching workspaces appeared; refresh workspace list")
			}
			if id != "" {
				return protocol.MustMarshal(struct {
					WorkspaceID string `json:"workspace_id"`
				}{id}), nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("cmux workspace created but its ID was not returned; refresh workspace list")
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil, errors.New("cmux workspace created but its ID was not returned; refresh workspace list")
}

type cmuxWorkspaceIdentity struct {
	Title     string `json:"title"`
	Directory string `json:"current_directory"`
}

func (m *CmuxManager) workspaceIDs(ctx context.Context) (map[string]cmuxWorkspaceIdentity, error) {
	raw, err := m.rpc(ctx, "mobile.workspace.list", cmuxRPCParams{Password: cmuxPassword()})
	if err != nil {
		return nil, err
	}
	var result struct {
		Workspaces []struct {
			ID string `json:"id"`
			cmuxWorkspaceIdentity
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Workspaces == nil {
		return nil, errors.New("cmux workspace list unavailable")
	}
	identities := make(map[string]cmuxWorkspaceIdentity, len(result.Workspaces))
	for _, workspace := range result.Workspaces {
		if _, err := uuid.Parse(workspace.ID); err == nil {
			identities[workspace.ID] = workspace.cmuxWorkspaceIdentity
		}
	}
	return identities, nil
}

func (m *CmuxManager) rpc(ctx context.Context, method string, params cmuxRPCParams) (json.RawMessage, error) {
	socketPath, err := m.socketPath()
	if err != nil {
		return nil, err
	}
	timeout := 5 * time.Second
	if method == "mobile.terminal.paste" || method == "browser.screenshot" {
		timeout = 12 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, errors.New("无法连接本机 cmux socket；请检查 cmux 是否运行及 CF Client 的合法连接权限")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	wire, err := json.Marshal(cmuxRPCRequest{ID: uuid.NewString(), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(wire, '\n')); err != nil {
		return nil, errors.New("cmux socket 写入失败")
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxCmuxResponseBytes+1)).ReadBytes('\n')
	if err != nil {
		return nil, errors.New("cmux socket 响应超时或连接中断")
	}
	if len(line) > maxCmuxResponseBytes {
		return nil, errors.New("cmux response too large")
	}
	var response cmuxRPCResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("invalid cmux response: %w", err)
	}
	if !response.OK {
		var failure struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(response.Error, &failure)
		if failure.Code == "method_not_found" || failure.Code == "unknown_method" ||
			strings.Contains(strings.ToLower(failure.Message), "unknown method") {
			return nil, errCmuxMethodUnavailable
		}
		var text string
		_ = json.Unmarshal(response.Error, &text)
		if strings.Contains(strings.ToLower(text), "unknown method") {
			return nil, errCmuxMethodUnavailable
		}
		// 错误对象可能包含内部数据，不原样转发到 Web。
		return nil, errors.New("cmux rejected request or access denied")
	}
	return response.Result, nil
}

func (m *CmuxManager) socketPath() (string, error) {
	if configured := m.config.SocketPath; configured != "" {
		if !filepath.IsAbs(configured) {
			return "", errors.New("cmux socket_path must be absolute")
		}
		return configured, nil
	}
	if configured := os.Getenv("CMUX_SOCKET_PATH"); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", errors.New("CMUX_SOCKET_PATH must be absolute")
		}
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	stateDir := filepath.Join(home, ".local", "state", "cmux")
	if recorded, err := os.ReadFile(filepath.Join(stateDir, "last-socket-path")); err == nil {
		candidate := strings.TrimSpace(string(recorded))
		if filepath.IsAbs(candidate) && socketExists(candidate) {
			return candidate, nil
		}
	}
	byUID := filepath.Join(stateDir, "cmux-"+strconv.Itoa(os.Getuid())+".sock")
	if socketExists(byUID) {
		return byUID, nil
	}
	return filepath.Join(stateDir, "cmux.sock"), nil
}

func socketExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func (c *Client) optionalFeatures() []string {
	if c.cmux == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.cmux.Execute(ctx, protocol.CmuxRequestPayload{
		RequestID: "startup-health", Action: protocol.CmuxHealth,
	})
	if err != nil {
		// 不影响原聊天、微信/飞书；新入口只在确实有 cmux socket 权限时出现。
		log.Printf("cmux 未就绪，新 Web 入口暂不展示；请从 cmux 终端启动 Client 后重连：%v", err)
		return nil
	}
	return []string{"cmux"}
}

// cmux 同一个 surface 的请求必须按 WebSocket 收到的顺序执行，否则连续
// 按键可能乱序，尤其是 CLI 原生审批的选择与回车。不同 surface 不相互阻塞。
type cmuxRequestQueue struct {
	client *Client
	mu     sync.Mutex
	lanes  map[string]chan *protocol.Message
	owners map[string]cmuxLeaseOwner
}

type cmuxLeaseOwner struct {
	viewerID string
	revision uint64
}

func newCmuxRequestQueue(client *Client) *cmuxRequestQueue {
	return &cmuxRequestQueue{
		client: client, lanes: make(map[string]chan *protocol.Message),
		owners: make(map[string]cmuxLeaseOwner),
	}
}

func (q *cmuxRequestQueue) updateOwner(change protocol.CmuxOwnershipPayload) {
	if !protocol.ValidCmuxSurfaceID(change.SurfaceID) || change.Revision == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if previous := q.owners[change.SurfaceID]; change.Revision >= previous.revision {
		q.owners[change.SurfaceID] = cmuxLeaseOwner{viewerID: change.ViewerID, revision: change.Revision}
	}
}

func expectedCmuxLeaseSurface(request protocol.CmuxRequestPayload) string {
	switch request.Action {
	case protocol.CmuxWorkspaceRename, protocol.CmuxWorkspaceCreate,
		protocol.CmuxWorkspaceClose, protocol.CmuxWorkspaceReorder,
		protocol.CmuxGroupCreate, protocol.CmuxGroupRename,
		protocol.CmuxGroupAdd, protocol.CmuxGroupRemove,
		protocol.CmuxTerminalCreate, protocol.CmuxNotificationRead,
		protocol.CmuxBrowserNavigate, protocol.CmuxBrowserClick, protocol.CmuxBrowserType,
		protocol.CmuxBrowserKey, protocol.CmuxBrowserScroll, protocol.CmuxBrowserNav,
		protocol.CmuxBrowserScreenshot:
		return protocol.CmuxAdminSurfaceID
	case protocol.CmuxText, protocol.CmuxKey, protocol.CmuxPaste,
		protocol.CmuxScroll, protocol.CmuxMouse, protocol.CmuxTerminalClose,
		protocol.CmuxViewportReport, protocol.CmuxViewportClear:
		return request.SurfaceID
	case protocol.CmuxUploadBegin, protocol.CmuxUploadChunk, protocol.CmuxUploadFinish:
		if request.UploadKind == "file" {
			return protocol.CmuxAdminSurfaceID
		}
		return request.SurfaceID
	default:
		return ""
	}
}

func (q *cmuxRequestQueue) currentOwner(request protocol.CmuxRequestPayload, from string) bool {
	expected := expectedCmuxLeaseSurface(request)
	if expected == "" || from == "" && request.Action == protocol.CmuxViewportClear {
		return true // 内部清理请求无 Web 来源；文件读取/上传中止不依赖租约。
	}
	if request.LeaseSurfaceID != expected || request.LeaseRevision == 0 || request.ViewerID == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	owner := q.owners[expected]
	return owner.viewerID == request.ViewerID && owner.revision == request.LeaseRevision
}

func (q *cmuxRequestQueue) enqueue(msg *protocol.Message) {
	if len(msg.Payload) > protocol.MaxCmuxRequestPayloadBytes {
		return
	}
	var request protocol.CmuxRequestPayload
	if err := msg.ParsePayload(&request); err != nil || protocol.ValidateCmuxRequest(request) != nil {
		return
	}
	laneID := request.SurfaceID
	switch request.Action {
	case protocol.CmuxSnapshot, protocol.CmuxText, protocol.CmuxKey, protocol.CmuxPaste,
		protocol.CmuxScroll, protocol.CmuxMouse, protocol.CmuxTerminalClose,
		protocol.CmuxViewportReport, protocol.CmuxViewportClear:
		// 同一终端上的快照、按键和粘贴严格按到达顺序串行。
	case protocol.CmuxBrowserPorts, protocol.CmuxBrowserNavigate, protocol.CmuxBrowserScreenshot,
		protocol.CmuxBrowserClick, protocol.CmuxBrowserType, protocol.CmuxBrowserKey,
		protocol.CmuxBrowserScroll, protocol.CmuxBrowserNav:
		laneID = "__cmux_browser"
	default:
		laneID = "__cmux_metadata"
	}
	q.mu.Lock()
	lane := q.lanes[laneID]
	if lane == nil && len(q.lanes) < 64 {
		lane = make(chan *protocol.Message, 8)
		q.lanes[laneID] = lane
		go q.runLane(q.client.ctx, laneID, lane)
	}
	if lane == nil {
		q.mu.Unlock()
		q.client.sendCmuxFailure(msg.From, request.RequestID, "cmux 请求过多")
		return
	}
	select {
	case lane <- msg:
		q.mu.Unlock()
	default:
		q.mu.Unlock()
		q.client.sendCmuxFailure(msg.From, request.RequestID, "终端请求队列已满")
	}
}

func (q *cmuxRequestQueue) runLane(ctx context.Context, laneID string, lane chan *protocol.Message) {
	idle := time.NewTimer(45 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-lane:
			var request protocol.CmuxRequestPayload
			if msg.ParsePayload(&request) != nil || !q.currentOwner(request, msg.From) {
				if request.RequestID != "" && msg.From != "" {
					q.client.sendCmuxFailure(msg.From, request.RequestID, "终端所有权已变更，请重试")
				}
				continue
			}
			q.client.handleCmuxRequest(ctx, msg, func() bool { return q.currentOwner(request, msg.From) })
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(45 * time.Second)
		case <-idle.C:
			q.mu.Lock()
			if len(lane) == 0 {
				delete(q.lanes, laneID)
				q.mu.Unlock()
				return
			}
			q.mu.Unlock()
			idle.Reset(45 * time.Second)
		}
	}
}

func (c *Client) handleCmuxRequest(parent context.Context, msg *protocol.Message, leaseCurrent func() bool) {
	var request protocol.CmuxRequestPayload
	if err := msg.ParsePayload(&request); err != nil {
		return
	}
	timeout := 15 * time.Second
	if protocol.IsCmuxBrowserAction(request.Action) {
		timeout = 35 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	result, err := c.cmux.ExecuteWithLease(ctx, request, leaseCurrent)
	if (request.Action == protocol.CmuxScroll || request.Action == protocol.CmuxSnapshot) &&
		(time.Since(started) >= 100*time.Millisecond || err != nil) {
		log.Printf("[cmux-rpc] action=%s duration_ms=%d failed=%t", request.Action,
			time.Since(started).Milliseconds(), err != nil)
	}
	response := protocol.CmuxResponsePayload{RequestID: request.RequestID, Result: result}
	if err != nil {
		response.Result = nil
		response.Error = err.Error()
	}
	c.sendCmuxResult(ctx, msg.From, response)
}

func (c *Client) sendCmuxFailure(to, requestID, reason string) {
	c.sendCmuxResult(c.ctx, to, protocol.CmuxResponsePayload{RequestID: requestID, Error: reason})
}

func (c *Client) sendCmuxResult(ctx context.Context, to string, response protocol.CmuxResponsePayload) {
	out, _ := protocol.NewMessage(protocol.TypeCmuxResponse, response)
	out.To = to
	select {
	case c.send <- out:
	case <-ctx.Done():
	}
}
