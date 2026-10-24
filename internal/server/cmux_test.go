package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func testCmuxHandler() (*Handler, *Connection, *Connection) {
	hub := NewHub()
	client := &Connection{ID: "client-1", Type: ConnTypeClient, Features: []string{"cmux"}, Send: make(chan *protocol.Message, 8)}
	user := &Connection{ID: "user-1", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	hub.clients[client.ID] = client
	hub.users[user.ID] = user
	return NewHandler(hub, NewAuth([]string{"test"}), nil), user, client
}

func nextCmuxRequest(t *testing.T, client *Connection) *protocol.Message {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-client.Send:
			if message.Type == protocol.TypeCmuxRequest {
				return message
			}
			if message.Type != protocol.TypeCmuxOwnership {
				t.Fatalf("unexpected Client control message: %s", message.Type)
			}
		case <-timer.C:
			t.Fatal("expected cmux request never reached Client")
		}
	}
}

func TestCmuxInputClaimTakeoverAndRelease(t *testing.T) {
	handler, first, client := testCmuxHandler()
	second := &Connection{ID: "user-2", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	handler.hub.users[second.ID] = second
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(first, attach)
	handler.handleMessage(second, attach)
	<-first.Send
	<-second.Send

	claim, _ := protocol.NewMessage(protocol.TypeCmuxClaim, protocol.CmuxClaimPayload{SurfaceID: "s"})
	handler.handleMessage(first, claim)
	assertClaimState(t, <-first.Send, true, true)
	assertClaimState(t, <-second.Send, false, true)

	input, _ := protocol.NewMessage(protocol.TypeCmuxRequest,
		protocol.CmuxRequestPayload{RequestID: "one", Action: protocol.CmuxText, SurfaceID: "s", Text: "a"})
	handler.handleMessage(first, input)
	nextCmuxRequest(t, client)
	input, _ = protocol.NewMessage(protocol.TypeCmuxRequest,
		protocol.CmuxRequestPayload{RequestID: "two", Action: protocol.CmuxText, SurfaceID: "s", Text: "b"})
	handler.handleMessage(second, input)
	if (<-second.Send).Type != protocol.TypeCmuxResponse || len(client.Send) != 0 {
		t.Fatal("second device typed without input ownership")
	}
	handler.handleMessage(second, claim)
	assertClaimState(t, <-second.Send, false, true)
	if len(first.Send) != 0 {
		t.Fatal("plain claim stole input ownership")
	}

	forced, _ := protocol.NewMessage(protocol.TypeCmuxClaim,
		protocol.CmuxClaimPayload{SurfaceID: "s", Force: true})
	handler.handleMessage(second, forced)
	assertClaimState(t, <-second.Send, true, true)
	assertClaimState(t, <-first.Send, false, true)
	handler.handleMessage(first, input)
	if (<-first.Send).Type != protocol.TypeCmuxResponse {
		t.Fatal("old owner was allowed to type after takeover")
	}
	release, _ := protocol.NewMessage(protocol.TypeCmuxRelease,
		protocol.CmuxClaimPayload{SurfaceID: "s"})
	handler.handleMessage(second, release)
	assertClaimState(t, <-second.Send, false, false)
	assertClaimState(t, <-first.Send, false, false)
	handler.handleMessage(first, claim)
	assertClaimState(t, <-first.Send, true, true)
	assertClaimState(t, <-second.Send, false, true)
}

func TestCmuxAdminTakeoverWaitsForUnfinishedWorkspaceWrite(t *testing.T) {
	handler, first, client := testCmuxHandler()
	second := &Connection{ID: "user-2", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	handler.hub.users[second.ID] = second
	if !handler.hub.AttachCmuxUser(first.ID, client.ID) ||
		!handler.hub.AttachCmuxUser(second.ID, client.ID) {
		t.Fatal("cannot attach users")
	}
	firstClaim, ok := handler.hub.ClaimCmuxInput(first.ID, protocol.CmuxAdminSurfaceID, false)
	if !ok || !firstClaim.Owned {
		t.Fatal("first user must acquire management")
	}
	request := protocol.CmuxRequestPayload{RequestID: "slow-create", Action: protocol.CmuxWorkspaceCreate,
		Title: "isolated", Directory: "/tmp", Profile: "claude"}
	message, _ := protocol.NewMessage(protocol.TypeCmuxRequest, request)
	if !handler.hub.ForwardCmuxRequest(first.ID, request, message) {
		t.Fatal("create request was not forwarded")
	}
	nextCmuxRequest(t, client)
	blocked, ok := handler.hub.ClaimCmuxInput(second.ID, protocol.CmuxAdminSurfaceID, true)
	if !ok || blocked.Owned || !blocked.Occupied {
		t.Fatalf("takeover acknowledged before old write returned: %+v", blocked)
	}
	reply, _ := protocol.NewMessage(protocol.TypeCmuxResponse,
		protocol.CmuxResponsePayload{RequestID: request.RequestID})
	reply.To = first.ID
	if !handler.hub.DeliverCmuxResponse(client, reply) {
		t.Fatal("old write response missing")
	}
	<-first.Send
	taken, ok := handler.hub.ClaimCmuxInput(second.ID, protocol.CmuxAdminSurfaceID, true)
	if !ok || !taken.Owned {
		t.Fatalf("takeover remained blocked after response: %+v", taken)
	}
}

func assertClaimState(t *testing.T, msg *protocol.Message, owned, occupied bool) {
	t.Helper()
	if msg.Type != protocol.TypeCmuxClaimed {
		t.Fatalf("expected cmux_claimed, got %s", msg.Type)
	}
	var state protocol.CmuxClaimedPayload
	if err := msg.ParsePayload(&state); err != nil || state.Owned != owned ||
		state.Occupied != occupied || state.SurfaceID != "s" {
		t.Fatalf("unexpected cmux claim state: %+v %v", state, err)
	}
}

func TestCmuxViewportRequiresOwnerAndKeepsGenerationAfterRelease(t *testing.T) {
	handler, owner, client := testCmuxHandler()
	other := &Connection{ID: "user-2", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	handler.hub.users[other.ID] = other
	if !handler.hub.AttachCmuxUser(owner.ID, client.ID) ||
		!handler.hub.AttachCmuxUser(other.ID, client.ID) {
		t.Fatal("cannot attach cmux test users")
	}
	request := func(id string) (protocol.CmuxRequestPayload, *protocol.Message) {
		payload := protocol.CmuxRequestPayload{
			RequestID: id, Action: protocol.CmuxViewportReport, SurfaceID: "s",
			Columns: 100, Rows: 30, Generation: 1, ViewerID: "forged",
		}
		message, _ := protocol.NewMessage(protocol.TypeCmuxRequest, payload)
		return payload, message
	}
	payload, message := request("before")
	if handler.hub.ForwardCmuxRequest(owner.ID, payload, message) {
		t.Fatal("viewport report acquired input without an explicit claim")
	}
	if _, ok := handler.hub.ClaimCmuxInput(owner.ID, "s", false); !ok {
		t.Fatal("owner claim failed")
	}
	payload, message = request("first")
	if handler.hub.ForwardCmuxRequest(other.ID, payload, message) {
		t.Fatal("non-owner reported viewport")
	}
	payload, message = request("second")
	if !handler.hub.ForwardCmuxRequest(owner.ID, payload, message) {
		t.Fatal("owner report failed")
	}
	var sent protocol.CmuxRequestPayload
	if err := nextCmuxRequest(t, client).ParsePayload(&sent); err != nil ||
		sent.ViewerID != owner.ID || sent.Generation != 1 {
		t.Fatalf("server did not bind viewer/generation: %+v err=%v", sent, err)
	}
	if _, ok := handler.hub.ReleaseCmuxInput(owner.ID, "s"); !ok {
		t.Fatal("release failed")
	}
	if err := nextCmuxRequest(t, client).ParsePayload(&sent); err != nil ||
		sent.Action != protocol.CmuxViewportClear || sent.Generation != 2 ||
		sent.ViewerID != owner.ID {
		t.Fatalf("release did not clear previous viewport: %+v err=%v", sent, err)
	}
	if _, ok := handler.hub.ClaimCmuxInput(owner.ID, "s", false); !ok {
		t.Fatal("reclaim failed")
	}
	payload, message = request("third")
	if !handler.hub.ForwardCmuxRequest(owner.ID, payload, message) {
		t.Fatal("reclaimed owner report failed")
	}
	if err := nextCmuxRequest(t, client).ParsePayload(&sent); err != nil || sent.Generation != 3 {
		t.Fatalf("reclaimed viewport generation regressed: %+v err=%v", sent, err)
	}
}

func TestCmuxSnapshotViewportRequiresOwnerAndExpiresWithoutStickyReport(t *testing.T) {
	handler, owner, client := testCmuxHandler()
	if !handler.hub.AttachCmuxUser(owner.ID, client.ID) {
		t.Fatal("cannot attach cmux user")
	}
	snapshot := protocol.CmuxRequestPayload{
		RequestID: "snapshot", Action: protocol.CmuxSnapshot, SurfaceID: "s",
		Columns: 90, Rows: 28,
	}
	message, _ := protocol.NewMessage(protocol.TypeCmuxRequest, snapshot)
	if handler.hub.ForwardCmuxRequest(owner.ID, snapshot, message) {
		t.Fatal("snapshot viewport acquired input without claim")
	}
	if _, ok := handler.hub.ClaimCmuxInput(owner.ID, "s", false); !ok {
		t.Fatal("claim failed")
	}
	if (<-client.Send).Type != protocol.TypeCmuxOwnership {
		t.Fatal("ownership marker missing")
	}
	message, _ = protocol.NewMessage(protocol.TypeCmuxRequest, snapshot)
	if !handler.hub.ForwardCmuxRequest(owner.ID, snapshot, message) {
		t.Fatal("owned snapshot viewport rejected")
	}
	var sent protocol.CmuxRequestPayload
	if err := nextCmuxRequest(t, client).ParsePayload(&sent); err != nil ||
		sent.Action != protocol.CmuxSnapshot || sent.ViewerID != owner.ID ||
		sent.Generation != 1 || sent.Columns != 90 || sent.Rows != 28 {
		t.Fatalf("snapshot viewport was not server-bound: %+v err=%v", sent, err)
	}
	if _, ok := handler.hub.ReleaseCmuxInput(owner.ID, "s"); !ok {
		t.Fatal("release failed")
	}
	if err := nextCmuxRequest(t, client).ParsePayload(&sent); err != nil ||
		sent.Action != protocol.CmuxViewportClear || sent.Generation != 2 ||
		sent.ViewerID != owner.ID {
		t.Fatalf("release did not clear replay viewport: %+v err=%v", sent, err)
	}
}

func TestCmuxViewportClearRetriesFullClientQueue(t *testing.T) {
	handler, user, client := testCmuxHandler()
	if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
		t.Fatal("cmux attach failed")
	}
	if _, ok := handler.hub.ClaimCmuxInput(user.ID, "s", false); !ok {
		t.Fatal("claim failed")
	}
	key := cmuxLeaseKey(client.ID, "s")
	handler.hub.cmuxViewportGenerations[cmuxViewportKey(key, user.ID)] = 1
	lease := handler.hub.cmuxLeases[key]
	lease.generation = 1
	handler.hub.cmuxLeases[key] = lease
	if (<-client.Send).Type != protocol.TypeCmuxOwnership {
		t.Fatal("initial ownership marker missing")
	}
	for i := 0; i < cap(client.Send); i++ {
		client.Send <- &protocol.Message{Type: protocol.TypePing}
	}
	if _, ok := handler.hub.ReleaseCmuxInput(user.ID, "s"); !ok {
		t.Fatal("release failed")
	}
	<-client.Send
	timeout := time.After(time.Second)
	for {
		select {
		case message := <-client.Send:
			if message.Type != protocol.TypeCmuxRequest {
				continue
			}
			var request protocol.CmuxRequestPayload
			if err := message.ParsePayload(&request); err != nil ||
				request.Action != protocol.CmuxViewportClear || request.Generation != 2 {
				t.Fatalf("unexpected retry clear: %+v err=%v", request, err)
			}
			return
		case <-timeout:
			t.Fatal("viewport clear was dropped when client queue was full")
		}
	}
}

func TestCmuxUploadAttachesTrustedViewerAndRequiresTerminalLease(t *testing.T) {
	handler, user, client := testCmuxHandler()
	if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
		t.Fatal("attach failed")
	}
	request := protocol.CmuxRequestPayload{
		RequestID: "start", Action: protocol.CmuxUploadBegin, UploadKind: "attachment",
		ViewerID: "forged", WorkspaceID: "w", SurfaceID: "s",
		FileName: "a.png", Size: 3,
	}
	message, _ := protocol.NewMessage(protocol.TypeCmuxRequest, request)
	if handler.hub.ForwardCmuxRequest(user.ID, request, message) {
		t.Fatal("attachment uploaded without terminal ownership")
	}
	if _, ok := handler.hub.ClaimCmuxInput(user.ID, "s", false); !ok {
		t.Fatal("claim failed")
	}
	if !handler.hub.ForwardCmuxRequest(user.ID, request, message) {
		t.Fatal("owned attachment rejected")
	}
	var delivered protocol.CmuxRequestPayload
	if err := nextCmuxRequest(t, client).ParsePayload(&delivered); err != nil || delivered.ViewerID != user.ID {
		t.Fatalf("untrusted viewer reached Client: %+v %v", delivered, err)
	}
	if _, ok := handler.hub.ReleaseCmuxInput(user.ID, "s"); !ok {
		t.Fatal("release failed")
	}
	abort := protocol.CmuxRequestPayload{
		RequestID: "abort", Action: protocol.CmuxUploadAbort, UploadKind: "attachment",
		WorkspaceID: "w", SurfaceID: "s", UploadID: "upload-1", ViewerID: "forged",
	}
	message, _ = protocol.NewMessage(protocol.TypeCmuxRequest, abort)
	if !handler.hub.ForwardCmuxRequest(user.ID, abort, message) {
		t.Fatal("owner could not abort after losing terminal lease")
	}
	if err := nextCmuxRequest(t, client).ParsePayload(&delivered); err != nil ||
		delivered.ViewerID != user.ID || delivered.Action != protocol.CmuxUploadAbort {
		t.Fatalf("upload abort viewer not bound: %+v %v", delivered, err)
	}
	file := request
	file.RequestID, file.UploadKind, file.Path, file.WorkspaceID, file.SurfaceID =
		"file", "file", "/tmp", "", ""
	message, _ = protocol.NewMessage(protocol.TypeCmuxRequest, file)
	if handler.hub.ForwardCmuxRequest(user.ID, file, message) {
		t.Fatal("file manager upload without management lease")
	}
	if _, ok := handler.hub.ClaimCmuxInput(user.ID, protocol.CmuxAdminSurfaceID, false); !ok {
		t.Fatal("management claim failed")
	}
	if !handler.hub.ForwardCmuxRequest(user.ID, file, message) {
		t.Fatal("file manager upload with management lease was rejected")
	}
	if err := nextCmuxRequest(t, client).ParsePayload(&delivered); err != nil || delivered.ViewerID != user.ID {
		t.Fatalf("file upload viewer was forged: %+v %v", delivered, err)
	}
}

func TestCmuxWorkspaceChangesRequireExplicitManagementLease(t *testing.T) {
	handler, first, client := testCmuxHandler()
	second := &Connection{ID: "user-2", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	handler.hub.users[second.ID] = second
	for _, user := range []*Connection{first, second} {
		if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
			t.Fatal("attach failed")
		}
	}
	request := protocol.CmuxRequestPayload{
		RequestID: "close", Action: protocol.CmuxWorkspaceClose,
		WorkspaceID: "w", SurfaceID: "s",
	}
	send := func(user *Connection) bool {
		msg, _ := protocol.NewMessage(protocol.TypeCmuxRequest, request)
		return handler.hub.ForwardCmuxRequest(user.ID, request, msg)
	}
	if send(first) {
		t.Fatal("workspace closed without management lease")
	}
	if _, ok := handler.hub.ClaimCmuxInput(first.ID, protocol.CmuxAdminSurfaceID, false); !ok {
		t.Fatal("management claim failed")
	}
	if send(second) || !send(first) {
		t.Fatal("read-only device changed workspace")
	}
	nextCmuxRequest(t, client)
	reply, _ := protocol.NewMessage(protocol.TypeCmuxResponse,
		protocol.CmuxResponsePayload{RequestID: request.RequestID})
	reply.To = first.ID
	if !handler.hub.DeliverCmuxResponse(client, reply) {
		t.Fatal("previous workspace write response missing")
	}
	<-first.Send
	state, ok := handler.hub.ClaimCmuxInput(second.ID, protocol.CmuxAdminSurfaceID, true)
	if !ok || !state.Owned {
		t.Fatal("explicit management takeover failed")
	}
	request.RequestID = "close-after-takeover"
	if send(first) || !send(second) {
		t.Fatal("old management owner retained write access")
	}
}

func TestCmuxBrowserWritesRequireManagementLeaseAndReadsStayAttached(t *testing.T) {
	handler, owner, client := testCmuxHandler()
	reader := &Connection{ID: "user-2", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	handler.hub.users[reader.ID] = reader
	for _, user := range []*Connection{owner, reader} {
		if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
			t.Fatal("attach failed")
		}
	}
	send := func(user *Connection, id string, action protocol.CmuxAction) bool {
		request := protocol.CmuxRequestPayload{
			RequestID: id, Action: action, WorkspaceID: "w",
			Port: 3000, BrowserPath: "/",
		}
		msg, _ := protocol.NewMessage(protocol.TypeCmuxRequest, request)
		return handler.hub.ForwardCmuxRequest(user.ID, request, msg)
	}
	if !send(reader, "ports", protocol.CmuxBrowserPorts) {
		t.Fatal("attached reader cannot list ports")
	}
	nextCmuxRequest(t, client)
	if send(reader, "navigate-denied", protocol.CmuxBrowserNavigate) {
		t.Fatal("browser navigation must not bypass management lease")
	}
	if send(reader, "screenshot-denied", protocol.CmuxBrowserScreenshot) {
		t.Fatal("browser screenshot must not bypass management lease")
	}
	if _, ok := handler.hub.ClaimCmuxInput(owner.ID, protocol.CmuxAdminSurfaceID, false); !ok {
		t.Fatal("management claim failed")
	}
	if !send(owner, "navigate", protocol.CmuxBrowserNavigate) {
		t.Fatal("current owner cannot navigate")
	}
	delivered := nextCmuxRequest(t, client)
	var request protocol.CmuxRequestPayload
	if err := delivered.ParsePayload(&request); err != nil ||
		request.ViewerID != owner.ID || request.LeaseSurfaceID != protocol.CmuxAdminSurfaceID ||
		request.LeaseRevision == 0 {
		t.Fatalf("browser request missing owner revision: %+v %v", request, err)
	}
	if send(reader, "click-denied", protocol.CmuxBrowserClick) {
		t.Fatal("read-only viewer controlled browser")
	}
}

func TestCmuxRequestRejectsOversizedUnknownFields(t *testing.T) {
	handler, user, client := testCmuxHandler()
	if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
		t.Fatal("attach failed")
	}
	raw := []byte(`{"request_id":"huge","action":"list","unused":"` +
		strings.Repeat("x", protocol.MaxCmuxRequestPayloadBytes) + `"}`)
	var request protocol.CmuxRequestPayload
	if err := json.Unmarshal(raw, &request); err != nil ||
		protocol.ValidateCmuxRequest(request) != nil {
		t.Fatal("test payload should be structurally valid")
	}
	message := &protocol.Message{Type: protocol.TypeCmuxRequest, Payload: raw}
	if handler.hub.ForwardCmuxRequest(user.ID, request, message) || len(client.Send) != 0 {
		t.Fatal("oversized unrelated field reached Client")
	}
}

func TestCmuxLeaseExpiresAndRejectsInvalidClaim(t *testing.T) {
	handler, first, client := testCmuxHandler()
	second := &Connection{ID: "user-2", Type: ConnTypeUser, Send: make(chan *protocol.Message, 8)}
	handler.hub.users[second.ID] = second
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(first, attach)
	handler.handleMessage(second, attach)
	<-first.Send
	<-second.Send
	invalid, _ := protocol.NewMessage(protocol.TypeCmuxClaim,
		protocol.CmuxClaimPayload{SurfaceID: "bad\nsurface"})
	handler.handleMessage(first, invalid)
	if (<-first.Send).Type != protocol.TypeError {
		t.Fatal("invalid surface claim accepted")
	}
	claim, _ := protocol.NewMessage(protocol.TypeCmuxClaim, protocol.CmuxClaimPayload{SurfaceID: "s"})
	handler.handleMessage(first, claim)
	<-first.Send
	<-second.Send
	handler.hub.mu.Lock()
	leaseKey := cmuxLeaseKey(client.ID, "s")
	lease := handler.hub.cmuxLeases[leaseKey]
	lease.expiresAt = time.Now().Add(-time.Second)
	handler.hub.cmuxLeases[leaseKey] = lease
	handler.hub.mu.Unlock()
	handler.handleMessage(second, claim)
	assertClaimState(t, <-second.Send, true, true)
	assertClaimState(t, <-first.Send, false, true)
}

func TestCmuxInputDoesNotClaimUnownedSurfaceImplicitly(t *testing.T) {
	handler, user, client := testCmuxHandler()
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, attach)
	<-user.Send
	request, _ := protocol.NewMessage(protocol.TypeCmuxRequest, protocol.CmuxRequestPayload{
		RequestID: "not-claimed", Action: protocol.CmuxKey, SurfaceID: "surface-1", Key: "return",
	})
	handler.handleMessage(user, request)
	if got := <-user.Send; got.Type != protocol.TypeCmuxResponse || len(client.Send) != 0 {
		t.Fatalf("unclaimed input reached client: %+v", got)
	}
	if len(handler.hub.cmuxLeases) != 0 {
		t.Fatal("unclaimed input silently obtained ownership")
	}
	claim, _ := protocol.NewMessage(protocol.TypeCmuxClaim,
		protocol.CmuxClaimPayload{SurfaceID: "surface-1"})
	handler.handleMessage(user, claim)
	<-user.Send
	handler.handleMessage(user, request)
	if got := nextCmuxRequest(t, client); got.Type != protocol.TypeCmuxRequest {
		t.Fatalf("claimed input was rejected: %+v", got)
	}
}

func TestCmuxAttachDoesNotStartLegacyTerminal(t *testing.T) {
	handler, user, client := testCmuxHandler()
	msg, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, msg)
	if received := <-user.Send; received.Type != protocol.TypeCmuxAttached {
		t.Fatalf("unexpected attach response: %s", received.Type)
	}
	if len(client.Send) != 0 || len(handler.hub.attachMap) != 0 {
		t.Fatal("cmux attach touched legacy chat/tmux route")
	}
	request, _ := protocol.NewMessage(protocol.TypeCmuxRequest, protocol.CmuxRequestPayload{RequestID: "r", Action: protocol.CmuxList})
	handler.handleMessage(user, request)
	forwarded := <-client.Send
	if forwarded.Type != protocol.TypeCmuxRequest || forwarded.From != user.ID || forwarded.To != client.ID {
		t.Fatalf("bad cmux route: %+v", forwarded)
	}
	response, _ := protocol.NewMessage(protocol.TypeCmuxResponse, protocol.CmuxResponsePayload{RequestID: "r"})
	response.To = user.ID
	handler.handleMessage(client, response)
	if received := <-user.Send; received.Type != protocol.TypeCmuxResponse {
		t.Fatalf("unexpected response: %s", received.Type)
	}
}

func TestCmuxRouteRejectsUnauthenticatedTargets(t *testing.T) {
	handler, user, client := testCmuxHandler()
	request, _ := protocol.NewMessage(protocol.TypeCmuxRequest, protocol.CmuxRequestPayload{RequestID: "r", Action: protocol.CmuxList})
	handler.handleMessage(user, request)
	var errorResponse protocol.CmuxResponsePayload
	if err := (<-user.Send).ParsePayload(&errorResponse); err != nil || errorResponse.Error == "" {
		t.Fatalf("missing not-attached error: %v", err)
	}
	if len(client.Send) != 0 {
		t.Fatal("unauthorized request was forwarded")
	}
	// 客户端无法伪造 attach；其他客户端的结果也不能投递给该用户。
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(client, attach)
	if len(client.Send) != 0 {
		t.Fatal("client forged user attach")
	}
	handler.handleMessage(user, attach)
	<-user.Send
	other := &Connection{ID: "client-2", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	reply, _ := protocol.NewMessage(protocol.TypeCmuxResponse, protocol.CmuxResponsePayload{RequestID: "r"})
	reply.To = user.ID
	handler.handleMessage(other, reply)
	if len(user.Send) != 0 {
		t.Fatal("another client forged response")
	}
}

func TestCmuxAttachRequiresCapability(t *testing.T) {
	handler, user, client := testCmuxHandler()
	client.Features = nil
	msg, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, msg)
	response := <-user.Send
	if response.Type != protocol.TypeCmuxResponse {
		t.Fatalf("expected disabled response, got %s", response.Type)
	}
	if len(handler.hub.cmuxAttachMap) != 0 {
		t.Fatal("disabled client was attached")
	}
}

func TestCmuxRouteStopsWhenClientLosesCapability(t *testing.T) {
	handler, user, client := testCmuxHandler()
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, attach)
	<-user.Send
	client.Features = nil
	request, _ := protocol.NewMessage(protocol.TypeCmuxRequest, protocol.CmuxRequestPayload{RequestID: "r", Action: protocol.CmuxList})
	handler.handleMessage(user, request)
	var response protocol.CmuxResponsePayload
	if err := (<-user.Send).ParsePayload(&response); err != nil || response.Error == "" {
		t.Fatalf("expected capability error: %v", err)
	}
	if len(client.Send) != 0 {
		t.Fatal("request reached client after capability was removed")
	}
}

func TestCmuxRouteRejectsInvalidActionBeforeLocalSocket(t *testing.T) {
	handler, user, client := testCmuxHandler()
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, attach)
	<-user.Send
	for _, request := range []protocol.CmuxRequestPayload{
		{RequestID: "r", Action: "system.exec"},
		{RequestID: "r", Action: protocol.CmuxPaste, SurfaceID: "s", Text: ""},
	} {
		msg, _ := protocol.NewMessage(protocol.TypeCmuxRequest, request)
		handler.handleMessage(user, msg)
		response := <-user.Send
		if response.Type != protocol.TypeCmuxResponse || len(client.Send) != 0 {
			t.Fatalf("invalid cmux request forwarded: %+v", request)
		}
	}
}

func TestCmuxEventsOnlyReachUsersAttachedToCurrentClient(t *testing.T) {
	handler, user, client := testCmuxHandler()
	other := &Connection{ID: "other", Type: ConnTypeUser, Send: make(chan *protocol.Message, 2)}
	handler.hub.users[other.ID] = other
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, attach)
	<-user.Send

	event, _ := protocol.NewMessage(protocol.TypeCmuxEvent,
		protocol.CmuxEventPayload{Sequence: 5, Category: "terminal", SurfaceID: "s"})
	event.From, event.To = "spoofed", other.ID
	handler.handleMessage(client, event)
	delivered := <-user.Send
	if delivered.From != client.ID || delivered.To != "" || len(other.Send) != 0 {
		t.Fatalf("cmux event routed to wrong user: %+v", delivered)
	}
	handler.handleMessage(other, event)
	if len(user.Send) != 0 {
		t.Fatal("web user injected a cmux event")
	}
	handler.hub.clients[client.ID] = &Connection{ID: client.ID, Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	handler.handleMessage(client, event)
	if len(user.Send) != 0 {
		t.Fatal("stale client injected a cmux event")
	}
}

func TestCmuxEventBroadcastAnnouncesGapAfterViewerQueueOverflow(t *testing.T) {
	handler, user, client := testCmuxHandler()
	user.Send = make(chan *protocol.Message, 2)
	if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
		t.Fatal("attach failed")
	}
	event, _ := protocol.NewMessage(protocol.TypeCmuxEvent,
		protocol.CmuxEventPayload{Sequence: 1, Category: "program"})
	user.Send <- event
	user.Send <- event
	handler.hub.BroadcastCmuxEvent(client, event)
	if !handler.hub.cmuxEventGaps[user.ID] {
		t.Fatal("lost event not recorded for this viewer")
	}
	<-user.Send
	<-user.Send
	handler.hub.BroadcastCmuxEvent(client, event)
	var gap, recovered protocol.CmuxEventPayload
	_ = (<-user.Send).ParsePayload(&gap)
	_ = (<-user.Send).ParsePayload(&recovered)
	if !gap.Gap || !gap.Resync || recovered.Sequence != 1 ||
		handler.hub.cmuxEventGaps[user.ID] {
		t.Fatalf("broadcast did not recover: %+v %+v", gap, recovered)
	}
}

func TestCmuxProgramEventsAreBoundedAndOtherPayloadsDoNotLeak(t *testing.T) {
	handler, user, client := testCmuxHandler()
	if !handler.hub.AttachCmuxUser(user.ID, client.ID) {
		t.Fatal("attach failed")
	}
	progress := 50
	records := make([]protocol.CmuxProgramRecord, 18)
	for i := range records {
		records[i] = protocol.CmuxProgramRecord{State: "working",
			Title: strings.Repeat("x", 300), Message: strings.Repeat("<", 512),
			Progress: &progress}
	}
	event, _ := protocol.NewMessage(protocol.TypeCmuxEvent, protocol.CmuxEventPayload{
		Name: "program_status.changed", Category: "program", WorkspaceID: "w",
		SurfaceID: "s", Programs: records,
	})
	if len(event.Payload) <= 24<<10 {
		t.Fatal("expected a valid large program event beyond old 24 KiB limit")
	}
	handler.handleCmuxEvent(client, event)
	var delivered protocol.CmuxEventPayload
	select {
	case message := <-user.Send:
		if err := message.ParsePayload(&delivered); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("program progress event not delivered")
	}
	if len(delivered.Programs) != 16 || len(delivered.Programs[0].Title) != 256 {
		t.Fatalf("program metadata not bounded: %+v", delivered)
	}
	leak, _ := protocol.NewMessage(protocol.TypeCmuxEvent, protocol.CmuxEventPayload{
		Name: "agent.hook.Notification", Category: "agent", Programs: records,
	})
	handler.handleCmuxEvent(client, leak)
	delivered = protocol.CmuxEventPayload{}
	if message := <-user.Send; message.ParsePayload(&delivered) != nil || len(delivered.Programs) != 0 {
		t.Fatalf("Hook metadata leaked to viewer: %+v", delivered)
	}
}

func TestCmuxResponseRequiresMatchingOutstandingRequest(t *testing.T) {
	handler, user, client := testCmuxHandler()
	attach, _ := protocol.NewMessage(protocol.TypeCmuxAttach, protocol.AttachPayload{ClientID: client.ID})
	handler.handleMessage(user, attach)
	<-user.Send
	response, _ := protocol.NewMessage(protocol.TypeCmuxResponse,
		protocol.CmuxResponsePayload{RequestID: "not-requested"})
	response.To = user.ID
	handler.handleMessage(client, response)
	if len(user.Send) != 0 {
		t.Fatal("unsolicited cmux response reached user")
	}
	request, _ := protocol.NewMessage(protocol.TypeCmuxRequest,
		protocol.CmuxRequestPayload{RequestID: "r1", Action: protocol.CmuxList})
	handler.handleMessage(user, request)
	<-client.Send
	handler.handleMessage(user, request)
	if (<-user.Send).Type != protocol.TypeCmuxResponse || len(client.Send) != 0 {
		t.Fatal("duplicate in-flight cmux request was forwarded")
	}
	response, _ = protocol.NewMessage(protocol.TypeCmuxResponse,
		protocol.CmuxResponsePayload{RequestID: "r1"})
	response.To = user.ID
	handler.handleMessage(client, response)
	if (<-user.Send).Type != protocol.TypeCmuxResponse {
		t.Fatal("matching response was not delivered")
	}
	handler.handleMessage(client, response)
	if len(user.Send) != 0 {
		t.Fatal("replayed cmux response reached user")
	}
}
