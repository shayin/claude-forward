package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func TestHandleShareProxiesClientResponse(t *testing.T) {
	hub := NewHub()
	client := &Connection{ID: "client-a", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	hub.clients[client.ID] = client
	handler := NewHandler(hub, nil, nil)
	go func() {
		request := <-client.Send
		var payload protocol.FileRequestPayload
		if err := request.ParsePayload(&payload); err != nil {
			t.Errorf("parse request: %v", err)
			return
		}
		response, _ := protocol.NewMessage(protocol.TypeFileResponse, protocol.FileResponsePayload{RequestID: payload.RequestID, StatusCode: http.StatusOK, ContentType: "text/css", Content: []byte("body{}")})
		handler.handleMessage(client, response)
	}()
	req := httptest.NewRequest(http.MethodGet, "/share/client-a/token/assets/site.css", nil)
	recorder := httptest.NewRecorder()
	handler.HandleShare(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "body{}" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/css" {
		t.Fatalf("content type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}
}

func TestHandleShareRejectsInvalidPathAndOfflineClient(t *testing.T) {
	handler := NewHandler(NewHub(), nil, nil)
	for _, target := range []string{"/share/client/token/../secret", "/share/client/token/%2e%2e/secret"} {
		recorder := httptest.NewRecorder()
		handler.HandleShare(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", target, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	handler.HandleShare(recorder, httptest.NewRequest(http.MethodGet, "/share/offline/token/index.html", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline status = %d", recorder.Code)
	}
}

func TestHandleShareTimesOutWhenClientDoesNotRespond(t *testing.T) {
	hub := NewHub()
	hub.clients["slow"] = &Connection{ID: "slow", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	handler := NewHandler(hub, nil, nil)
	oldTimeout := shareRequestTimeout
	shareRequestTimeout = time.Millisecond
	defer func() { shareRequestTimeout = oldTimeout }()
	recorder := httptest.NewRecorder()
	handler.HandleShare(recorder, httptest.NewRequest(http.MethodGet, "/share/slow/token/index.html", nil))
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d", recorder.Code)
	}
}
