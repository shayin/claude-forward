package server

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
)

var shareRequestTimeout = 20 * time.Second

type shareRequestBroker struct {
	mu      sync.Mutex
	pending map[string]chan protocol.FileResponsePayload
}

func newShareRequestBroker() *shareRequestBroker {
	return &shareRequestBroker{pending: make(map[string]chan protocol.FileResponsePayload)}
}

func (b *shareRequestBroker) request(ctx context.Context, client *Connection, token, filePath string) (protocol.FileResponsePayload, bool) {
	id := uuid.NewString()
	responseCh := make(chan protocol.FileResponsePayload, 1)
	b.mu.Lock()
	b.pending[id] = responseCh
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, id); b.mu.Unlock() }()
	msg, _ := protocol.NewMessage(protocol.TypeFileRequest, protocol.FileRequestPayload{RequestID: id, Token: token, Path: filePath})
	select {
	case client.Send <- msg:
	case <-ctx.Done():
		return protocol.FileResponsePayload{}, false
	}
	select {
	case response := <-responseCh:
		return response, true
	case <-ctx.Done():
		return protocol.FileResponsePayload{}, false
	}
}

func (b *shareRequestBroker) resolve(response protocol.FileResponsePayload) {
	b.mu.Lock()
	responseCh := b.pending[response.RequestID]
	b.mu.Unlock()
	if responseCh != nil {
		select {
		case responseCh <- response:
		default:
		}
	}
}

// HandleShare 处理 /share/<client-id>/<token>/<relative-path> 的反向静态代理。
func (h *Handler) HandleShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/share/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	clientID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	token, err := url.PathUnescape(parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fileParts := parts[2:]
	for i := range fileParts {
		fileParts[i], err = url.PathUnescape(fileParts[i])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if fileParts[i] == "" || fileParts[i] == "." || fileParts[i] == ".." {
			http.NotFound(w, r)
			return
		}
	}
	filePath := path.Join(fileParts...)
	if filePath == "." || strings.HasPrefix(filePath, "../") {
		http.NotFound(w, r)
		return
	}
	client, ok := h.hub.GetClient(clientID)
	if !ok {
		http.Error(w, "Client offline", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), shareRequestTimeout)
	defer cancel()
	response, ok := h.shareBroker.request(ctx, client, token, filePath)
	if !ok {
		http.Error(w, "Share request timed out", http.StatusGatewayTimeout)
		return
	}
	if response.StatusCode != http.StatusOK {
		statusCode := response.StatusCode
		if statusCode < http.StatusBadRequest || statusCode > 599 {
			statusCode = http.StatusBadGateway
		}
		http.Error(w, http.StatusText(statusCode), statusCode)
		return
	}
	w.Header().Set("Content-Type", response.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(response.Content)))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write(response.Content)
	}
}
