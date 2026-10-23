package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/shayin/claude-forward/internal/protocol"
	"golang.org/x/sys/unix"
)

// 网页服务从本机 cmux 浏览器打开，云端与 Web 只收到截图而非本机 HTTP 直连。
// 初始 URL 必须来自工作区侦测的端口；运行中的网页仍能自行跳转，
// 因此这个端口列表不是网络沙箱。后续地址核验仅尽力阻止离站画面，
// Browser 的实际访问边界是 CF 认证和独立的 Web 管理权。
func (m *CmuxManager) browserPorts(ctx context.Context, workspaceID string) ([]int, error) {
	list, err := m.rpc(ctx, "mobile.workspace.list", cmuxRPCParams{Password: cmuxPassword()})
	if err != nil {
		return nil, err
	}
	var mobile struct {
		Workspaces []struct {
			ID    string          `json:"id"`
			Ports json.RawMessage `json:"listening_ports"`
		} `json:"workspaces"`
	}
	if json.Unmarshal(list, &mobile) != nil {
		return nil, errors.New("cmux 工作区列表格式错误")
	}
	var raw json.RawMessage
	found := false
	for _, workspace := range mobile.Workspaces {
		if workspace.ID == workspaceID {
			found = true
			raw = workspace.Ports
			break
		}
	}
	if !found {
		return nil, errors.New("cmux 工作区不存在")
	}
	// 当前 cmux 将侦测到的端口放在 sidebar 快照；旧版可能只放在 mobile 列表。
	sidebar, sidebarErr := m.rpc(ctx, "extension.sidebar.snapshot", cmuxRPCParams{Password: cmuxPassword()})
	if sidebarErr == nil {
		var snapshot struct {
			Workspaces []struct {
				ID    string          `json:"id"`
				Ports json.RawMessage `json:"listening_ports"`
			} `json:"workspaces"`
		}
		if json.Unmarshal(sidebar, &snapshot) == nil {
			for _, workspace := range snapshot.Workspaces {
				if workspace.ID == workspaceID && len(workspace.Ports) > 0 {
					raw = workspace.Ports
					break
				}
			}
		}
	}
	var values []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return []int{}, nil
	}
	seen := make(map[int]bool)
	ports := make([]int, 0, min(len(values), 20))
	for _, value := range values {
		var port int
		if json.Unmarshal(value, &port) != nil {
			var object struct {
				Port int `json:"port"`
			}
			if json.Unmarshal(value, &object) != nil {
				continue
			}
			port = object.Port
		}
		if port > 0 && port <= 65535 && !seen[port] && len(ports) < 20 {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	return ports, nil
}

func cmuxPassword() string { return os.Getenv("CMUX_SOCKET_PASSWORD") }

func (m *CmuxManager) browserSurfaceBelongsTo(ctx context.Context, workspaceID, surfaceID string) (bool, error) {
	raw, err := m.rpc(ctx, "mobile.workspace.list", cmuxRPCParams{Password: cmuxPassword()})
	if err != nil {
		return false, err
	}
	var result struct {
		Workspaces []struct {
			ID       string `json:"id"`
			Surfaces []struct {
				ID        string `json:"id"`
				SurfaceID string `json:"surface_id"`
			} `json:"surfaces"`
		} `json:"workspaces"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false, errors.New("cmux 工作区列表格式错误")
	}
	for _, workspace := range result.Workspaces {
		for _, surface := range workspace.Surfaces {
			if surface.ID == surfaceID || surface.SurfaceID == surfaceID {
				return workspace.ID == workspaceID, nil
			}
		}
	}
	return false, nil
}

func (m *CmuxManager) validateBrowserLocation(ctx context.Context, workspaceID string, params cmuxRPCParams) error {
	params.Script = "window.location.href"
	raw, err := m.rpc(ctx, "browser.eval", params)
	if err != nil {
		return err
	}
	var location string
	if json.Unmarshal(raw, &location) != nil {
		var value struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return errors.New("无法核验 cmux 浏览器地址")
		}
		location = value.Value
	}
	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" ||
		parsed.User != nil {
		return errors.New("网页已离开允许的本机服务，已停止预览")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return errors.New("网页已离开允许的本机服务，已停止预览")
	}
	freshPorts, err := m.browserPorts(ctx, workspaceID)
	if err != nil {
		return err
	}
	current := false
	for _, fresh := range freshPorts {
		current = current || fresh == port
	}
	if !current {
		return errors.New("本机网页服务已停止或更换工作区，请重新选择端口")
	}
	for _, allowed := range m.browserAllowedPorts[workspaceID] {
		if port == allowed {
			return nil
		}
	}
	return errors.New("网页已离开允许的本机服务，已停止预览")
}

func (m *CmuxManager) executeBrowserRequest(ctx context.Context, request protocol.CmuxRequestPayload,
	leaseCurrent func() bool) (json.RawMessage, error) {
	// 浏览器操作由独立队列串行；互斥还防止直接调用 manager 时两个工作区复用错误的 surface。
	m.browserMu.Lock()
	defer m.browserMu.Unlock()

	if request.Action == protocol.CmuxBrowserPorts {
		ports, err := m.browserPorts(ctx, request.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return protocol.MustMarshal(struct {
			Ports []int `json:"ports"`
		}{ports}), nil
	}
	params := cmuxRPCParams{Password: cmuxPassword()}
	write := func(method string, params cmuxRPCParams) (json.RawMessage, error) {
		if leaseCurrent != nil && !leaseCurrent() {
			return nil, errors.New("网页管理权已变更，请重新操作")
		}
		return m.rpc(ctx, method, params)
	}
	surface := m.browsers[request.WorkspaceID]
	if request.Action == protocol.CmuxBrowserNavigate {
		ports, err := m.browserPorts(ctx, request.WorkspaceID)
		if err != nil {
			return nil, err
		}
		allowed := false
		for _, port := range ports {
			allowed = allowed || port == request.Port
		}
		if !allowed {
			return nil, errors.New("网页端口未在当前工作区的 cmux 服务列表中")
		}
		if surface == "" {
			params.WorkspaceID = request.WorkspaceID
			tab, err := write("browser.tab.new", params)
			if err != nil {
				return nil, err
			}
			var created struct {
				SurfaceID   string `json:"surface_id"`
				WorkspaceID string `json:"workspace_id"`
			}
			if json.Unmarshal(tab, &created) != nil || !protocol.ValidCmuxSurfaceID(created.SurfaceID) {
				return nil, errors.New("cmux 未返回有效的浏览器标签")
			}
			if created.WorkspaceID != "" && created.WorkspaceID != request.WorkspaceID {
				return nil, errors.New("cmux 浏览器标签创建在其他工作区")
			}
			if created.WorkspaceID == "" {
				verified, err := m.browserSurfaceBelongsTo(ctx, request.WorkspaceID, created.SurfaceID)
				if err != nil || !verified {
					return nil, errors.New("无法核验 cmux 浏览器标签的工作区归属")
				}
			}
			surface = created.SurfaceID
			m.browsers[request.WorkspaceID] = surface
		}
		params.SurfaceID = surface
		params.URL = "http://127.0.0.1:" + strconv.Itoa(request.Port) + request.BrowserPath
		if _, err := write("browser.navigate", params); err != nil {
			delete(m.browsers, request.WorkspaceID)
			return nil, err
		}
		m.browserAllowedPorts[request.WorkspaceID] = ports
	} else if surface == "" {
		return nil, errors.New("尚未打开当前工作区的网页服务")
	}
	params = cmuxRPCParams{Password: cmuxPassword(), SurfaceID: surface}
	if request.Action != protocol.CmuxBrowserNavigate {
		if err := m.validateBrowserLocation(ctx, request.WorkspaceID, params); err != nil {
			return nil, err
		}
	}
	switch request.Action {
	case protocol.CmuxBrowserNavigate, protocol.CmuxBrowserScreenshot:
		// 下方统一截图；轮询不触碰本机服务的响应正文和 Cookie。
	case protocol.CmuxBrowserClick:
		// 固定脚本只嵌入通过协议校验的 [0,1] 数字，不运行客户端传来的 JS。
		params.Script = fmt.Sprintf(`(() => {
			const px = Math.round(innerWidth * %g), py = Math.round(innerHeight * %g);
			const el = document.elementFromPoint(px, py);
			if (!el) return "no-element";
			el.focus?.();
			const options = {bubbles:true,cancelable:true,view:window,clientX:px,clientY:py};
			for (const name of ["mousedown","mouseup","click"]) el.dispatchEvent(new MouseEvent(name, options));
			return "ok";
		})()`, request.X, request.Y)
		if _, err := write("browser.eval", params); err != nil {
			return nil, err
		}
	case protocol.CmuxBrowserType:
		text, _ := json.Marshal(request.Text) // JSON 字符串转义后作为唯一动态脚本字面量。
		params.Script = `(() => {
			const el = document.activeElement;
			if (!el || el === document.body) return "no-focus";
			const text = ` + string(text) + `;
			if (el.isContentEditable) { document.execCommand("insertText", false, text); return "ok"; }
			if (!("value" in el)) return "unsupported";
			const start = el.selectionStart ?? el.value.length;
			const end = el.selectionEnd ?? el.value.length;
			el.value = el.value.slice(0, start) + text + el.value.slice(end);
			try { el.setSelectionRange?.(start + text.length, start + text.length); } catch {}
			el.dispatchEvent(new Event("input", {bubbles:true}));
			el.dispatchEvent(new Event("change", {bubbles:true}));
			return "ok";
		})()`
		if _, err := write("browser.eval", params); err != nil {
			return nil, err
		}
	case protocol.CmuxBrowserKey:
		params.Key = request.Key
		if _, err := write("browser.press", params); err != nil {
			return nil, err
		}
	case protocol.CmuxBrowserScroll:
		params.DY = request.ScrollY
		if _, err := write("browser.scroll", params); err != nil {
			return nil, err
		}
	case protocol.CmuxBrowserNav:
		method := map[string]string{"back": "browser.back", "forward": "browser.forward", "reload": "browser.reload"}[request.Key]
		if _, err := write(method, params); err != nil {
			return nil, err
		}
	}
	if request.Action != protocol.CmuxBrowserScreenshot {
		if err := m.validateBrowserLocation(ctx, request.WorkspaceID, cmuxRPCParams{
			Password: cmuxPassword(), SurfaceID: surface,
		}); err != nil {
			return nil, err
		}
	}
	shot, err := write("browser.screenshot", params)
	if err != nil {
		return nil, err
	}
	var image struct {
		PNGBase64 string `json:"png_base64"`
		Data      string `json:"data"`
		Base64    string `json:"base64"`
		Path      string `json:"path"`
	}
	if json.Unmarshal(shot, &image) != nil {
		_ = json.Unmarshal(shot, &image.Data)
	}
	reapCmuxBrowserScreenshot(image.Path, os.TempDir())
	// 截图与地址查询是 cmux 的两个 RPC，无法做到真正原子化；再核验一次可
	// 阻止查询后发生的持续跳转，以及接管期间旧管理者收到已拍到的画面。
	if leaseCurrent != nil && !leaseCurrent() {
		return nil, errors.New("网页管理权已变更，请重新操作")
	}
	if err := m.validateBrowserLocation(ctx, request.WorkspaceID, params); err != nil {
		return nil, err
	}
	encoded := image.PNGBase64
	if encoded == "" {
		encoded = image.Data
	}
	if encoded == "" {
		encoded = image.Base64
	}
	if len(encoded) > 12<<20 {
		return nil, errors.New("网页截图超出 9 MiB 限制")
	}
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(content) < 8 || string(content[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, errors.New("cmux 未返回有效的 PNG 网页截图")
	}
	return protocol.MustMarshal(struct {
		Data string `json:"data"`
	}{encoded}), nil
}

func reapCmuxBrowserScreenshot(path, tempDir string) {
	// cmux 会将每帧截图另存到 TMPDIR；只清理其直属截图目录内的普通文件。
	// 不信任 RPC 返回的任意路径，也不跟随符号链接。
	if path != "" {
		screenshotDir := filepath.Join(tempDir, "cmux-browser-screenshots")
		if filepath.Dir(filepath.Clean(path)) == screenshotDir {
			dir, err := unix.Open(screenshotDir,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return
			}
			defer unix.Close(dir)
			name := filepath.Base(path)
			var stat unix.Stat_t
			if unix.Fstatat(dir, name, &stat, unix.AT_SYMLINK_NOFOLLOW) == nil &&
				stat.Mode&unix.S_IFMT == unix.S_IFREG {
				_ = unix.Unlinkat(dir, name, 0)
			}
		}
	}
}
