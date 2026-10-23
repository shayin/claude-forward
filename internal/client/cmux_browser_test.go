package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shayin/claude-forward/internal/protocol"
)

func TestCmuxBrowserPortsAndWhitelistedNavigation(t *testing.T) {
	var calls []cmuxRPCRequest
	encoded := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls = append(calls, request)
		switch request.Method {
		case "mobile.workspace.list":
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[4000]}]}}`
		case "extension.sidebar.snapshot":
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[8080,{"port":3000},3000,0,70000]}]}}`
		case "browser.tab.new":
			return `{"ok":true,"result":{"surface_id":"browser-1","workspace_id":"w"}}`
		case "browser.eval":
			return `{"ok":true,"result":{"value":"http://127.0.0.1:3000/page"}}`
		case "browser.screenshot":
			return `{"ok":true,"result":{"png_base64":"` + encoded + `"}}`
		default:
			return `{"ok":true,"result":{}}`
		}
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	ports, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "ports", Action: protocol.CmuxBrowserPorts, WorkspaceID: "w",
	})
	if err != nil || string(ports) != `{"ports":[3000,8080]}` {
		t.Fatalf("ports = %s, %v", ports, err)
	}
	prior := len(calls)
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "denied", Action: protocol.CmuxBrowserNavigate, WorkspaceID: "w",
		Port: 4000, BrowserPath: "/",
	})
	if err == nil || len(calls) != prior+2 {
		t.Fatalf("unlisted port must not navigate: %v, calls=%v", err, calls[prior:])
	}
	got, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "open", Action: protocol.CmuxBrowserNavigate, WorkspaceID: "w",
		Port: 3000, BrowserPath: "/page?name=中文",
	})
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(got, &image) != nil || image.Data != encoded {
		t.Fatalf("image = %s", got)
	}
	var navigated bool
	for _, call := range calls {
		if call.Method == "browser.navigate" {
			navigated = call.Params.URL == "http://127.0.0.1:3000/page?name=中文" &&
				call.Params.SurfaceID == "browser-1"
		}
	}
	if !navigated {
		t.Fatalf("missing loopback navigation: %v", calls)
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "click", Action: protocol.CmuxBrowserClick, WorkspaceID: "w",
		X: 0.5, Y: 0.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	var clicked bool
	for _, call := range calls {
		if call.Method == "browser.eval" && strings.Contains(call.Params.Script, "elementFromPoint") {
			clicked = strings.Contains(call.Params.Script, "innerWidth * 0.5") &&
				strings.Contains(call.Params.Script, "innerHeight * 0.25") &&
				strings.Contains(call.Params.Script, "clientX:px,clientY:py")
		}
	}
	if !clicked {
		t.Fatal("browser click lost event coordinates")
	}
	prior = len(calls)
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "again", Action: protocol.CmuxBrowserScreenshot, WorkspaceID: "w",
	})
	var screenCalls []string
	for _, call := range calls[prior:] {
		if call.Method == "browser.eval" || call.Method == "browser.screenshot" {
			screenCalls = append(screenCalls, call.Method)
		}
	}
	if err != nil || len(screenCalls) != 3 ||
		screenCalls[0] != "browser.eval" || screenCalls[1] != "browser.screenshot" ||
		screenCalls[2] != "browser.eval" {
		t.Fatalf("screenshot must reuse existing surface: %v %v", err, calls[prior:])
	}
}

func TestCmuxBrowserStopsPreviewWhenWorkspacePortDisappears(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
	running := true
	shots := 0
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		switch request.Method {
		case "mobile.workspace.list":
			if !running {
				return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[]}]}}`
			}
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[3000]}]}}`
		case "extension.sidebar.snapshot":
			return `{"ok":false,"error":{"code":"method_not_found"}}`
		case "browser.tab.new":
			return `{"ok":true,"result":{"surface_id":"browser-1","workspace_id":"w"}}`
		case "browser.eval":
			return `{"ok":true,"result":{"value":"http://127.0.0.1:3000/"}}`
		case "browser.screenshot":
			shots++
			return `{"ok":true,"result":{"png_base64":"` + encoded + `"}}`
		}
		return `{"ok":true,"result":{}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "open", Action: protocol.CmuxBrowserNavigate, WorkspaceID: "w", Port: 3000,
	})
	if err != nil || shots != 1 {
		t.Fatalf("initial browser navigation: %v, shots=%d", err, shots)
	}
	running = false
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "reload", Action: protocol.CmuxBrowserNav, WorkspaceID: "w", Key: "reload",
	})
	if err == nil || shots != 1 {
		t.Fatalf("stale workspace port must not be reloaded or screenshotted: %v, shots=%d", err, shots)
	}
}

func TestCmuxBrowserDiscardsScreenshotAfterRedirectOrLeaseTakeover(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
	var lease atomic.Bool
	lease.Store(true)
	redirect := false
	takeover := false
	location := "http://127.0.0.1:3000/"
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		switch request.Method {
		case "mobile.workspace.list":
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[3000]}]}}`
		case "extension.sidebar.snapshot":
			return `{"ok":false,"error":{"code":"method_not_found"}}`
		case "browser.tab.new":
			return `{"ok":true,"result":{"surface_id":"browser-1","workspace_id":"w"}}`
		case "browser.eval":
			return `{"ok":true,"result":{"value":"` + location + `"}}`
		case "browser.screenshot":
			if redirect {
				location = "https://example.com/"
			}
			if takeover {
				lease.Store(false)
			}
			return `{"ok":true,"result":{"data":"` + encoded + `"}}`
		default:
			return `{"ok":true,"result":{}}`
		}
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	_, err := manager.ExecuteWithLease(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "open", Action: protocol.CmuxBrowserNavigate, WorkspaceID: "w", Port: 3000,
	}, lease.Load)
	if err != nil {
		t.Fatal(err)
	}
	redirect = true
	_, err = manager.ExecuteWithLease(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "shot", Action: protocol.CmuxBrowserScreenshot, WorkspaceID: "w",
	}, lease.Load)
	if err == nil || !strings.Contains(err.Error(), "已离开") {
		t.Fatalf("screenshot after redirect must be discarded: %v", err)
	}
	location = "http://127.0.0.1:3000/"
	redirect, takeover = false, true
	_, err = manager.ExecuteWithLease(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "takeover", Action: protocol.CmuxBrowserScreenshot, WorkspaceID: "w",
	}, lease.Load)
	if err == nil || !strings.Contains(err.Error(), "管理权已变更") {
		t.Fatalf("screenshot after lease takeover must be discarded: %v", err)
	}
}

func TestCmuxBrowserRejectsUntrustedActionsAndInvalidImage(t *testing.T) {
	var calls []cmuxRPCRequest
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls = append(calls, request)
		switch request.Method {
		case "mobile.workspace.list":
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[3000]}]}}`
		case "extension.sidebar.snapshot":
			return `{"ok":false,"error":{"code":"method_not_found"}}`
		case "browser.tab.new":
			return `{"ok":true,"result":{"surface_id":"browser-1","workspace_id":"w"}}`
		case "browser.eval":
			return `{"ok":true,"result":{"value":"http://127.0.0.1:3000/"}}`
		case "browser.screenshot":
			return `{"ok":true,"result":{"data":"not-a-png"}}`
		default:
			return `{"ok":true,"result":{}}`
		}
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "missing", Action: protocol.CmuxBrowserScreenshot, WorkspaceID: "w",
	})
	if err == nil || len(calls) != 0 {
		t.Fatalf("screenshot before navigation must be rejected without RPC: %v", err)
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "missing-workspace", Action: protocol.CmuxBrowserNavigate, WorkspaceID: "other", Port: 3000,
	})
	if err == nil {
		t.Fatal("unknown workspace must not navigate")
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "open", Action: protocol.CmuxBrowserNavigate, WorkspaceID: "w", Port: 3000,
	})
	if err == nil || !strings.Contains(err.Error(), "PNG") {
		t.Fatalf("invalid screenshot must fail: %v", err)
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "type", Action: protocol.CmuxBrowserType, WorkspaceID: "w",
		Text: `"; window.location="https://evil.example"; //`,
	})
	if err == nil {
		t.Fatal("invalid screenshot must fail after typing")
	}
	for _, call := range calls {
		if call.Method == "browser.eval" {
			if strings.Contains(call.Params.Script, `const text = ""; window.location=`) {
				t.Fatalf("unescaped user text in fixed browser script: %s", call.Params.Script)
			}
			return
		}
	}
	t.Fatal("expected browser.eval with JSON-escaped text")
}

func TestCmuxBrowserRejectsOldManagerBeforeNextWriteRPC(t *testing.T) {
	var lease atomic.Bool
	lease.Store(true)
	queryStarted := make(chan struct{})
	releaseQuery := make(chan struct{})
	var mu sync.Mutex
	var methods []string
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		mu.Lock()
		methods = append(methods, request.Method)
		mu.Unlock()
		switch request.Method {
		case "mobile.workspace.list":
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[3000]}]}}`
		case "extension.sidebar.snapshot":
			close(queryStarted)
			<-releaseQuery
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[3000]}]}}`
		}
		return `{"ok":true,"result":{}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	complete := make(chan error, 1)
	go func() {
		_, err := manager.ExecuteWithLease(context.Background(), protocol.CmuxRequestPayload{
			RequestID: "stale", Action: protocol.CmuxBrowserNavigate,
			WorkspaceID: "w", Port: 3000,
		}, lease.Load)
		complete <- err
	}()
	<-queryStarted
	lease.Store(false) // 另一台设备在端口查询期间接管管理权。
	close(releaseQuery)
	if err := <-complete; err == nil || !strings.Contains(err.Error(), "管理权") {
		t.Fatalf("stale browser write was not cancelled: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range methods {
		if method == "browser.tab.new" || method == "browser.navigate" {
			t.Fatalf("stale request performed a browser write: %v", methods)
		}
	}
}

func TestCmuxBrowserRejectsWrongWorkspaceAndRedirect(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
	var tabWorkspace = "other"
	var finalURL = "http://127.0.0.1:3000/"
	var calls []cmuxRPCRequest
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls = append(calls, request)
		switch request.Method {
		case "mobile.workspace.list":
			return `{"ok":true,"result":{"workspaces":[{"id":"w","listening_ports":[3000]}]}}`
		case "extension.sidebar.snapshot":
			return `{"ok":false,"error":{"code":"method_not_found"}}`
		case "browser.tab.new":
			return `{"ok":true,"result":{"surface_id":"browser-1","workspace_id":"` + tabWorkspace + `"}}`
		case "browser.eval":
			if request.Params.Script == "window.location.href" {
				return `{"ok":true,"result":{"value":"` + finalURL + `"}}`
			}
		case "browser.screenshot":
			return `{"ok":true,"result":{"data":"` + encoded + `"}}`
		}
		return `{"ok":true,"result":{}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	navigate := protocol.CmuxRequestPayload{RequestID: "nav", Action: protocol.CmuxBrowserNavigate,
		WorkspaceID: "w", Port: 3000}
	if _, err := manager.Execute(context.Background(), navigate); err == nil ||
		!strings.Contains(err.Error(), "其他工作区") {
		t.Fatalf("wrong workspace accepted: %v", err)
	}
	for _, call := range calls {
		if call.Method == "browser.navigate" {
			t.Fatal("browser navigation reached another workspace")
		}
	}
	tabWorkspace = "w"
	finalURL = "https://example.com/redirect"
	if _, err := manager.Execute(context.Background(), navigate); err == nil ||
		!strings.Contains(err.Error(), "已离开") {
		t.Fatalf("external redirect screenshot accepted: %v", err)
	}
	for _, call := range calls {
		if call.Method == "browser.screenshot" {
			t.Fatal("external URL screenshot reached remote viewer")
		}
	}
	finalURL = "http://127.0.0.1:3000/"
	if _, err := manager.Execute(context.Background(), navigate); err != nil {
		t.Fatalf("reopening allowed service failed: %v", err)
	}
	finalURL = "http://127.0.0.1:9999/"
	if _, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "shot", Action: protocol.CmuxBrowserScreenshot, WorkspaceID: "w",
	}); err == nil {
		t.Fatal("later off-port navigation was still captured")
	}
}

func TestReapCmuxBrowserScreenshotOnlyRemovesTrustedRegularFile(t *testing.T) {
	temp := t.TempDir()
	dir := filepath.Join(temp, "cmux-browser-screenshots")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "shot.png")
	outside := filepath.Join(temp, "other.png")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("image"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reapCmuxBrowserScreenshot(outside, temp)
	reapCmuxBrowserScreenshot(inside, temp)
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("trusted screenshot not removed: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was removed: %v", err)
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	reapCmuxBrowserScreenshot(link, temp)
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was followed or removed: %v", err)
	}
}
