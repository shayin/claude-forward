package client

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func fakeCmuxSocket(t *testing.T, reply func(cmuxRPCRequest) string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "cmux.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var request cmuxRPCRequest
				if json.Unmarshal(line, &request) == nil {
					_, _ = conn.Write([]byte(reply(request) + "\n"))
				}
			}()
		}
	}()
	return path
}

func TestCmuxManagerUsesOnlyAllowedRPC(t *testing.T) {
	type expectedCall struct {
		method string
		params cmuxRPCParams
	}
	calls := make(chan expectedCall, 6)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- expectedCall{request.Method, request.Params}
		return `{"ok":true,"result":{"workspaces":[]}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	t.Setenv("CMUX_SOCKET_PASSWORD", "local-only")
	cases := []struct {
		action protocol.CmuxAction
		method string
	}{
		{protocol.CmuxHealth, "system.ping"},
		{protocol.CmuxList, "mobile.workspace.list"},
		{protocol.CmuxSnapshot, "terminal.replay"},
		{protocol.CmuxText, "surface.send_text"},
		{protocol.CmuxKey, "surface.send_key"},
		{protocol.CmuxPaste, "mobile.terminal.paste"},
	}
	for _, testCase := range cases {
		result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
			RequestID: "r1", Action: testCase.action, SurfaceID: "surface:1", Text: "hello", Key: "return",
		})
		if err != nil || string(result) != `{"workspaces":[]}` {
			t.Fatalf("%s: result=%s err=%v", testCase.action, result, err)
		}
		call := <-calls
		if call.method != testCase.method || call.params.Password != "local-only" {
			t.Fatalf("%s: unexpected RPC %+v", testCase.action, call)
		}
		if testCase.action == protocol.CmuxList {
			sidebar := <-calls
			if sidebar.method != "extension.sidebar.snapshot" ||
				sidebar.params.Password != "local-only" {
				t.Fatalf("list sidebar merge: unexpected RPC %+v", sidebar)
			}
		}
		if testCase.action == protocol.CmuxPaste &&
			(call.params.TerminalID != "surface:1" || call.params.SubmitKey != "none") {
			t.Fatalf("paste target or submit key wrong: %+v", call.params)
		}
	}
}

func TestCmuxManagerRejectsInvalidRequestsBeforeSocket(t *testing.T) {
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: "/does/not/exist"})
	cases := []protocol.CmuxRequestPayload{
		{RequestID: "r", Action: "system.exec"},
		{RequestID: "r", Action: protocol.CmuxSnapshot},
		{RequestID: "r", Action: protocol.CmuxKey, SurfaceID: "s"},
		{RequestID: "r", Action: protocol.CmuxText, SurfaceID: "s", Text: strings.Repeat("x", 64<<10+1)},
		{RequestID: strings.Repeat("x", 129), Action: protocol.CmuxHealth},
	}
	for _, request := range cases {
		if _, err := manager.Execute(context.Background(), request); err == nil ||
			strings.Contains(err.Error(), "connect cmux socket") {
			t.Fatalf("invalid request reached socket: %+v, err=%v", request.Action, err)
		}
	}
}

func TestCmuxManagerViewportRPCParams(t *testing.T) {
	calls := make(chan cmuxRPCRequest, 2)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request
		return `{"ok":true,"result":{"columns":100,"rows":30}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	for _, action := range []protocol.CmuxAction{
		protocol.CmuxViewportReport, protocol.CmuxViewportClear,
	} {
		_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
			RequestID: string(action), Action: action, SurfaceID: "surface:1",
			ViewerID: "user-1", Columns: 100, Rows: 30, Generation: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		call := <-calls
		if call.Method != "terminal.viewport" || call.Params.ClientID != "cf:user-1" ||
			call.Params.SurfaceID != "surface:1" || call.Params.ViewportGeneration != 5 ||
			call.Params.Clear != (action == protocol.CmuxViewportClear) {
			t.Fatalf("invalid viewport parameters: %+v", call)
		}
		if action == protocol.CmuxViewportReport &&
			(call.Params.ViewportColumns != 100 || call.Params.ViewportRows != 30) {
			t.Fatalf("viewport dimensions missing: %+v", call)
		}
	}
}

func TestCmuxManagerSnapshotCarriesExpiringViewport(t *testing.T) {
	calls := make(chan cmuxRPCRequest, 1)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request
		return `{"ok":true,"result":{"columns":100,"rows":30}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "snapshot", Action: protocol.CmuxSnapshot, SurfaceID: "surface:1",
		ViewerID: "user-1", Columns: 100, Rows: 30, Generation: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := <-calls
	if call.Method != "terminal.replay" || call.Params.SurfaceID != "surface:1" ||
		call.Params.ClientID != "cf:user-1" || call.Params.ViewportColumns != 100 ||
		call.Params.ViewportRows != 30 || call.Params.ViewportGeneration != 7 ||
		call.Params.DeviceName != "Claude Forward Web" || call.Params.Clear {
		t.Fatalf("snapshot viewport parameters missing: %+v", call)
	}
}

func TestCmuxManagerRetriesSnapshotViewportTransition(t *testing.T) {
	calls := 0
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls++
		if calls == 1 {
			return `{"ok":false,"error":{"code":"viewport_transition","message":"resizing"}}`
		}
		return `{"ok":true,"result":{"columns":90,"rows":28}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "snapshot", Action: protocol.CmuxSnapshot, SurfaceID: "surface:1",
		ViewerID: "user-1", Columns: 90, Rows: 28, Generation: 1,
	})
	if err != nil || calls != 2 || !strings.Contains(string(result), `"columns":90`) {
		t.Fatalf("viewport transition was not retried: calls=%d result=%s err=%v", calls, result, err)
	}
}

func TestCmuxManagerSanitizesSocketError(t *testing.T) {
	socket := fakeCmuxSocket(t, func(cmuxRPCRequest) string {
		return `{"ok":false,"error":{"secret":"must-not-leak"}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{RequestID: "r", Action: protocol.CmuxList})
	if err == nil || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("error leaked socket data: %v", err)
	}
}

func TestCmuxManagerSocketConfiguration(t *testing.T) {
	manager := NewCmuxManager(CmuxConfig{SocketPath: "relative.sock"})
	if _, err := manager.socketPath(); err == nil {
		t.Fatal("relative socket path must be rejected")
	}
	socket := fakeCmuxSocket(t, func(cmuxRPCRequest) string { return `{"ok":true,"result":{}}` })
	manager = NewCmuxManager(CmuxConfig{SocketPath: socket})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := manager.Execute(ctx, protocol.CmuxRequestPayload{RequestID: "r", Action: protocol.CmuxHealth}); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledCmuxDoesNotTouchLegacyChat(t *testing.T) {
	client := NewClient(DefaultConfig())
	if len(client.optionalFeatures()) != 0 {
		t.Fatal("cmux advertised without opt-in")
	}
	msg, _ := protocol.NewMessage(protocol.TypeCmuxRequest,
		protocol.CmuxRequestPayload{RequestID: "r1", Action: protocol.CmuxList})
	msg.From = "web-user"
	client.handleMessage(msg)
	response := <-client.send
	if response.Type != protocol.TypeCmuxResponse || response.To != "web-user" {
		t.Fatalf("unexpected response: %+v", response)
	}
	var payload protocol.CmuxResponsePayload
	if err := response.ParsePayload(&payload); err != nil || payload.Error != "cmux disabled" {
		t.Fatalf("disabled cmux result: %+v, %v", payload, err)
	}
}

func TestCmuxFeatureIsAdvertisedOnlyWhenLocalSocketIsUsable(t *testing.T) {
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		if request.Method != "system.ping" {
			t.Errorf("unexpected startup method: %s", request.Method)
		}
		return `{"ok":true,"result":"pong"}`
	})
	config := DefaultConfig()
	config.Cmux = CmuxConfig{Enabled: true, SocketPath: socket}
	client := NewClient(config)
	features := client.optionalFeatures()
	if len(features) != 1 || features[0] != "cmux" {
		t.Fatalf("usable cmux socket must be advertised: %v", features)
	}
	config.Cmux.SocketPath = filepath.Join(t.TempDir(), "missing.sock")
	client = NewClient(config)
	if features := client.optionalFeatures(); len(features) != 0 {
		t.Fatalf("unusable cmux socket must leave legacy Client usable without feature: %v", features)
	}
}

func TestCmuxManagerMutationsUseConstrainedSocketMethods(t *testing.T) {
	calls := make(chan cmuxRPCRequest, 12)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request
		return `{"ok":true,"result":{}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	cases := []struct {
		request protocol.CmuxRequestPayload
		method  string
	}{
		{protocol.CmuxRequestPayload{Action: protocol.CmuxWorkspaceRename, WorkspaceID: "w", Title: "新工作区"}, "workspace.rename"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxWorkspaceClose, WorkspaceID: "w"}, "workspace.close"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxWorkspaceReorder, WorkspaceID: "w"}, "workspace.reorder"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxGroupCreate, Title: "项目组", Directory: "/tmp"}, "workspace.group.create"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxGroupRename, GroupID: "g", Title: "新组"}, "workspace.group.rename"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxGroupAdd, WorkspaceID: "w", GroupID: "g"}, "workspace.group.add"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxGroupRemove, WorkspaceID: "w"}, "workspace.group.remove"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxTerminalCreate, WorkspaceID: "w"}, "mobile.terminal.create"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxTerminalClose, WorkspaceID: "w", SurfaceID: "s"}, "surface.close"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxScroll, WorkspaceID: "w", SurfaceID: "s", DeltaLines: -3}, "mobile.terminal.scroll"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxMouse, WorkspaceID: "w", SurfaceID: "s"}, "mobile.terminal.mouse"},
		{protocol.CmuxRequestPayload{Action: protocol.CmuxNotifications}, "notification.list"},
	}
	for _, testCase := range cases {
		testCase.request.RequestID = "r"
		if _, err := manager.Execute(context.Background(), testCase.request); err != nil {
			t.Fatalf("%s: %v", testCase.method, err)
		}
		call := <-calls
		if call.Method != testCase.method {
			t.Fatalf("expected %s, got %s", testCase.method, call.Method)
		}
		if call.Method == "workspace.reorder" && (call.Params.Index == nil || *call.Params.Index != 0) {
			t.Fatal("zero index must be serialized")
		}
		if call.Method == "workspace.group.create" &&
			(call.Params.ChildWorkspaceIDs == nil || len(*call.Params.ChildWorkspaceIDs) != 0 ||
				call.Params.IdempotencyKey != "r") {
			t.Fatal("group create lost empty members or idempotency key")
		}
		if call.Method == "mobile.terminal.mouse" && (call.Params.Column == nil || call.Params.Row == nil) {
			t.Fatal("zero mouse coordinate must be serialized")
		}
		if call.Method == "mobile.terminal.scroll" {
			replay := <-calls
			if replay.Method != "terminal.replay" || replay.Params.SurfaceID != "s" {
				t.Fatalf("scroll replay target wrong: %+v", replay)
			}
		}
	}
}

func TestCmuxScrollReturnsReplayWithoutExtraWebRoundTrip(t *testing.T) {
	calls := make(chan string, 2)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request.Method
		if request.Method == "terminal.replay" {
			return `{"ok":true,"result":{"render_grid":{"active_screen":"alternate"}}}`
		}
		return `{"ok":true,"result":{"scrolled":true}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "scroll-frame", Action: protocol.CmuxScroll,
		WorkspaceID: "w", SurfaceID: "s", DeltaLines: -3,
	})
	if err != nil || string(result) != `{"render_grid":{"active_screen":"alternate"}}` {
		t.Fatalf("scroll should return replay: result=%s err=%v", result, err)
	}
	if first, second := <-calls, <-calls; first != "mobile.terminal.scroll" || second != "terminal.replay" {
		t.Fatalf("unexpected RPC order: %s then %s", first, second)
	}
}

func TestCmuxScrollSlowReplayFallsBackWithoutRepeatingInput(t *testing.T) {
	calls := make(chan string, 2)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request.Method
		if request.Method == "terminal.replay" {
			time.Sleep(900 * time.Millisecond)
		}
		return `{"ok":true,"result":{"scrolled":true}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	started := time.Now()
	result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "scroll-slow-replay", Action: protocol.CmuxScroll,
		WorkspaceID: "w", SurfaceID: "s", DeltaLines: 2,
	})
	if err != nil || string(result) != `{"scrolled":true}` {
		t.Fatalf("slow replay must retain scroll success: result=%s err=%v", result, err)
	}
	if time.Since(started) >= 1500*time.Millisecond {
		t.Fatal("slow replay blocked scroll too long")
	}
	if first, second := <-calls, <-calls; first != "mobile.terminal.scroll" || second != "terminal.replay" {
		t.Fatalf("scroll must execute once: %s then %s", first, second)
	}
}

func TestCmuxScrollFailureDoesNotTriggerReplay(t *testing.T) {
	calls := make(chan string, 1)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request.Method
		return `{"ok":false,"error":{"code":"unavailable","message":"not ready"}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "scroll-failure", Action: protocol.CmuxScroll,
		WorkspaceID: "w", SurfaceID: "s", DeltaLines: 2,
	})
	if err == nil {
		t.Fatal("failed scroll must report an error")
	}
	if first := <-calls; first != "mobile.terminal.scroll" {
		t.Fatalf("unexpected RPC: %s", first)
	}
	select {
	case extra := <-calls:
		t.Fatalf("failed scroll must not trigger replay: %s", extra)
	default:
	}
}

func TestCmuxClientOrdersInputPerSurface(t *testing.T) {
	calls := make(chan string, 3)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		if request.Params.Text == "first" {
			time.Sleep(30 * time.Millisecond)
		}
		calls <- request.Params.Text
		return `{"ok":true,"result":{}}`
	})
	config := DefaultConfig()
	config.Cmux = CmuxConfig{Enabled: true, SocketPath: socket}
	client := NewClient(config)
	owner, _ := protocol.NewMessage(protocol.TypeCmuxOwnership, protocol.CmuxOwnershipPayload{
		SurfaceID: "s", ViewerID: "user", Revision: 1,
	})
	client.handleMessage(owner)
	for _, text := range []string{"first", "second", "third"} {
		msg, _ := protocol.NewMessage(protocol.TypeCmuxRequest, protocol.CmuxRequestPayload{
			RequestID: text, Action: protocol.CmuxText, SurfaceID: "s", Text: text,
			ViewerID: "user", LeaseSurfaceID: "s", LeaseRevision: 1,
		})
		msg.From = "user"
		client.handleMessage(msg)
	}
	for _, expected := range []string{"first", "second", "third"} {
		select {
		case actual := <-calls:
			if actual != expected {
				t.Fatalf("input order: expected %s, got %s", expected, actual)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for cmux input")
		}
		select {
		case <-client.send:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for cmux response")
		}
	}
	client.cancel()
}

func TestCmuxClientDropsQueuedInputAfterTakeover(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	calls := make(chan string, 3)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		if request.Params.Text == "first" {
			close(started)
			<-unblock
		}
		calls <- request.Params.Text
		return `{"ok":true,"result":{}}`
	})
	config := DefaultConfig()
	config.Cmux = CmuxConfig{Enabled: true, SocketPath: socket}
	client := NewClient(config)
	defer client.cancel()
	sendOwner := func(viewer string, revision uint64) {
		t.Helper()
		message, _ := protocol.NewMessage(protocol.TypeCmuxOwnership, protocol.CmuxOwnershipPayload{
			SurfaceID: "s", ViewerID: viewer, Revision: revision,
		})
		client.handleMessage(message)
	}
	sendInput := func(text, viewer string, revision uint64) {
		t.Helper()
		message, _ := protocol.NewMessage(protocol.TypeCmuxRequest, protocol.CmuxRequestPayload{
			RequestID: text, Action: protocol.CmuxText, SurfaceID: "s", Text: text,
			ViewerID: viewer, LeaseSurfaceID: "s", LeaseRevision: revision,
		})
		message.From = viewer
		client.handleMessage(message)
	}
	sendOwner("old", 1)
	sendInput("first", "old", 1)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first input never started")
	}
	sendInput("stale", "old", 1)
	sendOwner("new", 2)
	sendInput("fresh", "new", 2)
	close(unblock)
	select {
	case call := <-calls:
		if call != "first" {
			t.Fatalf("unexpected first call %s", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not complete")
	}
	select {
	case call := <-calls:
		if call != "fresh" {
			t.Fatalf("stale queued input was executed: %s", call)
		}
	case <-time.After(time.Second):
		t.Fatal("new owner input did not execute")
	}
	select {
	case call := <-calls:
		t.Fatalf("extra cmux input executed: %s", call)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCmuxWorkspaceCreateUsesArgvAndRequiresDirectory(t *testing.T) {
	dir := t.TempDir()
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		if request.Method != "mobile.workspace.list" {
			t.Errorf("unexpected method before workspace creation: %s", request.Method)
		}
		return `{"ok":true,"result":{"workspaces":[]}}`
	})
	script := filepath.Join(dir, "fake-cmux")
	argsFile := filepath.Join(dir, "arguments")
	t.Setenv("CMUX_TEST_ARGS", argsFile)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CMUX_TEST_ARGS\"\necho 0b5dfc89-b0d6-4e18-8327-bac2febbf038\n"), 0700); err != nil {
		t.Fatal(err)
	}
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	manager.cliPath = script
	name := "开发 --command 'rm -rf /'"
	result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "r", Action: protocol.CmuxWorkspaceCreate, Title: name, Directory: dir,
	})
	if err != nil || string(result) != `{"workspace_id":"0b5dfc89-b0d6-4e18-8327-bac2febbf038"}` {
		t.Fatalf("create result=%s err=%v", result, err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--id-format\nuuids\nnew-workspace\n--name\n"+name+"\n--cwd\n"+dir+"\n--focus\ntrue\n" {
		t.Fatalf("cmux argv changed: %s", data)
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "r2", Action: protocol.CmuxWorkspaceCreate, Title: "Claude", Directory: dir,
		Profile: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(argsFile)
	if err != nil || string(data) != "--id-format\nuuids\nnew-workspace\n--name\nClaude\n--cwd\n"+dir+
		"\n--focus\nfalse\n--command\nclaude\n" {
		t.Fatalf("native CLI profile must not add permission-skip flags: %s %v", data, err)
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "r", Action: protocol.CmuxWorkspaceCreate, Title: "Bad", Directory: "/not-a-real-directory",
	})
	if err == nil || !strings.Contains(err.Error(), "directory unavailable") {
		t.Fatalf("nonexistent directory not rejected: %v", err)
	}
}

func TestCmuxWorkspaceCreateResolvesNativeShortRefWithoutSelectingAnExistingWorkspace(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "created")
	existingID := "0b5dfc89-b0d6-4e18-8327-bac2febbf038"
	newID := "1885a9d1-23be-4ed8-943a-33566f651ff0"
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		if request.Method != "mobile.workspace.list" {
			t.Errorf("unexpected method: %s", request.Method)
		}
		workspaces := []map[string]string{
			{"id": existingID, "title": "独立验收", "current_directory": dir},
		}
		if _, err := os.Stat(marker); err == nil {
			workspaces = append(workspaces, map[string]string{
				"id": newID, "title": "独立验收", "current_directory": dir,
			})
		}
		result, _ := json.Marshal(map[string]interface{}{"workspaces": workspaces})
		return `{"ok":true,"result":` + string(result) + `}`
	})
	script := filepath.Join(dir, "fake-cmux")
	t.Setenv("CMUX_CREATE_MARKER", marker)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+
		"touch \"$CMUX_CREATE_MARKER\"\necho 'OK workspace:26'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	manager.cliPath = script
	result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "create-short-ref", Action: protocol.CmuxWorkspaceCreate,
		Title: "独立验收", Directory: dir,
	})
	if err != nil || string(result) != `{"workspace_id":"`+newID+`"}` {
		t.Fatalf("native short ref should resolve to only the new UUID: result=%s err=%v", result, err)
	}
}

func TestCmuxWorkspaceCreateRejectsAmbiguousNativeShortRef(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "created")
	socket := fakeCmuxSocket(t, func(cmuxRPCRequest) string {
		workspaces := []map[string]string{}
		if _, err := os.Stat(marker); err == nil {
			for _, id := range []string{"1885a9d1-23be-4ed8-943a-33566f651ff0",
				"289b7b9c-2bd6-44d4-8b3c-524a1983b4a1"} {
				workspaces = append(workspaces, map[string]string{
					"id": id, "title": "同名工作区", "current_directory": dir,
				})
			}
		}
		result, _ := json.Marshal(map[string]interface{}{"workspaces": workspaces})
		return `{"ok":true,"result":` + string(result) + `}`
	})
	script := filepath.Join(dir, "fake-cmux")
	t.Setenv("CMUX_CREATE_MARKER", marker)
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$CMUX_CREATE_MARKER\"\necho 'OK workspace:26'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	manager.cliPath = script
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "ambiguous", Action: protocol.CmuxWorkspaceCreate,
		Title: "同名工作区", Directory: dir,
	})
	if err == nil || !strings.Contains(err.Error(), "multiple matching") {
		t.Fatalf("ambiguous new workspace identity must fail closed: %v", err)
	}
}

func TestCmuxManagerFallsBackOnlyForUnsupportedMethods(t *testing.T) {
	calls := make(chan string, 4)
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		calls <- request.Method
		if request.Method == "terminal.replay" {
			return `{"ok":false,"error":{"code":"method_not_found","message":"unknown method"}}`
		}
		return `{"ok":true,"result":{"text":"fallback"}}`
	})
	manager := NewCmuxManager(CmuxConfig{SocketPath: socket})
	result, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "r", Action: protocol.CmuxSnapshot, SurfaceID: "s",
	})
	if err != nil || string(result) != `{"text":"fallback"}` ||
		<-calls != "terminal.replay" || <-calls != "surface.read_text" {
		t.Fatalf("unsupported replay did not fall back: result=%s err=%v", result, err)
	}
}
