package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	cmuxDownloadChunkLimit = 256 << 10
	cmuxDownloadTotalLimit = 1 << 30
	cmuxDownloadIdle       = 45 * time.Second
	cmuxDownloadPendingTTL = 30 * time.Second
	cmuxDownloadPerToken   = 4
)

type cmuxDownloadPart struct {
	data []byte
	end  bool
	ack  chan error
}

// 下载内容仍只经已授权 WebSocket file_read 获取；Server 仅用一次性票据做有背压的 HTTP 中继。
type cmuxDownloadSession struct {
	name    string
	owner   [32]byte
	started chan struct{}
	done    chan struct{}
	parts   chan cmuxDownloadPart
	mu      sync.Mutex
	once    sync.Once
	claimed bool
	busy    bool
	written int64
	size    int64
	timer   *time.Timer
}

func (s *cmuxDownloadSession) close() {
	s.once.Do(func() { close(s.done) })
}

type cmuxDownloadBroker struct {
	mu       sync.Mutex
	sessions map[string]*cmuxDownloadSession
}

func newCmuxDownloadBroker() *cmuxDownloadBroker {
	return &cmuxDownloadBroker{sessions: make(map[string]*cmuxDownloadSession)}
}

func (b *cmuxDownloadBroker) remove(id string, session *cmuxDownloadSession) {
	b.mu.Lock()
	if b.sessions[id] == session {
		delete(b.sessions, id)
	}
	b.mu.Unlock()
	if session.timer != nil {
		session.timer.Stop()
	}
	session.close()
}

func (b *cmuxDownloadBroker) expirePending(id string, session *cmuxDownloadSession) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.claimed {
		return
	}
	b.remove(id, session)
}

func (b *cmuxDownloadBroker) get(id string) *cmuxDownloadSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

func downloadBearer(r *http.Request) string {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimPrefix(value, prefix)
}

func (h *Handler) HandleCmuxDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	const base = "/api/cmux-download"
	if r.URL.Path == base && r.Method == http.MethodPost {
		token := downloadBearer(r)
		if token == "" || !h.auth.ValidateToken(token) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		var input struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if json.NewDecoder(r.Body).Decode(&input) != nil ||
			!utf8.ValidString(input.Name) || input.Name == "" || len(input.Name) > 255 ||
			input.Size < 0 || input.Size > cmuxDownloadTotalLimit ||
			strings.ContainsAny(input.Name, "/\\\r\n\x00") || path.Base(input.Name) != input.Name {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		session := &cmuxDownloadSession{
			name: input.Name, size: input.Size, owner: sha256.Sum256([]byte(token)),
			started: make(chan struct{}), done: make(chan struct{}),
			parts: make(chan cmuxDownloadPart),
		}
		id := uuid.NewString()
		h.cmuxDownloads.mu.Lock()
		owned := 0
		for _, existing := range h.cmuxDownloads.sessions {
			if existing.owner == session.owner {
				owned++
			}
		}
		if len(h.cmuxDownloads.sessions) >= 32 || owned >= cmuxDownloadPerToken {
			h.cmuxDownloads.mu.Unlock()
			http.Error(w, "Too many downloads", http.StatusTooManyRequests)
			return
		}
		h.cmuxDownloads.sessions[id] = session
		session.timer = time.AfterFunc(cmuxDownloadPendingTTL,
			func() { h.cmuxDownloads.expirePending(id, session) })
		h.cmuxDownloads.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			ID string `json:"id"`
		}{id})
		return
	}
	if !strings.HasPrefix(r.URL.Path, base+"/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"/"), "/")
	if len(parts) < 1 || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	session := h.cmuxDownloads.get(id)
	if session == nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		h.streamCmuxDownload(w, r, id, session)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost ||
		(parts[1] != "chunk" && parts[1] != "finish" && parts[1] != "abort") {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := downloadBearer(r)
	if token == "" || !h.auth.ValidateToken(token) || sha256.Sum256([]byte(token)) != session.owner {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if parts[1] == "abort" {
		// 和流式响应写入持同一把锁；204 返回后不再开始新分片写入。
		session.mu.Lock()
		session.close()
		session.mu.Unlock()
		h.cmuxDownloads.remove(id, session)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.postCmuxDownloadPart(w, r, session, parts[1] == "finish")
}

func (h *Handler) postCmuxDownloadPart(w http.ResponseWriter, r *http.Request, session *cmuxDownloadSession, finish bool) {
	select {
	case <-session.started:
	case <-session.done:
		http.Error(w, "Download expired", http.StatusGone)
		return
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		http.Error(w, "Download not started", http.StatusGatewayTimeout)
		return
	}
	var bytes []byte
	if !finish {
		r.Body = http.MaxBytesReader(w, r.Body, cmuxDownloadChunkLimit)
		var err error
		bytes, err = io.ReadAll(r.Body)
		if err != nil || len(bytes) == 0 {
			http.Error(w, "Invalid chunk", http.StatusBadRequest)
			return
		}
	}
	session.mu.Lock()
	offset, err := strconv.ParseInt(r.Header.Get("X-Download-Offset"), 10, 64)
	if session.busy || err != nil || offset != session.written ||
		session.written+int64(len(bytes)) > session.size ||
		finish && session.written != session.size {
		session.mu.Unlock()
		http.Error(w, "Invalid download offset", http.StatusConflict)
		return
	}
	session.busy = true
	session.mu.Unlock()
	defer func() { session.mu.Lock(); session.busy = false; session.mu.Unlock() }()
	part := cmuxDownloadPart{data: bytes, end: finish, ack: make(chan error, 1)}
	timer := time.NewTimer(cmuxDownloadIdle)
	defer timer.Stop()
	select {
	case session.parts <- part:
	case <-session.done:
		http.Error(w, "Download expired", http.StatusGone)
		return
	case <-r.Context().Done():
		return
	case <-timer.C:
		http.Error(w, "Download stalled", http.StatusGatewayTimeout)
		return
	}
	select {
	case err = <-part.ack:
	case <-session.done:
		select {
		case err = <-part.ack:
		default:
			err = errors.New("download stopped")
		}
	case <-r.Context().Done():
		return
	case <-timer.C:
		err = errors.New("download stalled")
	}
	if err != nil {
		http.Error(w, "Download failed", http.StatusBadGateway)
		return
	}
	session.mu.Lock()
	session.written += int64(len(bytes))
	session.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) streamCmuxDownload(w http.ResponseWriter, r *http.Request, id string, session *cmuxDownloadSession) {
	session.mu.Lock()
	select {
	case <-session.done:
		session.mu.Unlock()
		http.Error(w, "Download expired", http.StatusGone)
		return
	default:
	}
	if session.claimed {
		session.mu.Unlock()
		http.Error(w, "Download already started", http.StatusGone)
		return
	}
	session.claimed = true
	if session.timer != nil {
		session.timer.Stop()
	}
	close(session.started)
	session.mu.Unlock()
	defer h.cmuxDownloads.remove(id, session)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+
		strings.ReplaceAll(url.QueryEscape(session.name), "+", "%20"))
	w.Header().Set("Content-Length", strconv.FormatInt(session.size, 10))
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(cmuxDownloadIdle))
	w.WriteHeader(http.StatusOK)
	if controller.Flush() != nil {
		return
	}
	if session.size == 0 {
		return
	}
	for {
		select {
		case part := <-session.parts:
			if part.end {
				part.ack <- nil
				return
			}
			session.mu.Lock()
			select {
			case <-session.done:
				session.mu.Unlock()
				part.ack <- errors.New("download stopped")
				return
			default:
			}
			_ = controller.SetWriteDeadline(time.Now().Add(cmuxDownloadIdle))
			count, err := w.Write(part.data)
			if err == nil && count != len(part.data) {
				err = io.ErrShortWrite
			}
			if err == nil {
				err = controller.Flush()
			}
			session.mu.Unlock()
			part.ack <- err
			if err != nil {
				return
			}
		case <-session.done:
			return
		case <-r.Context().Done():
			return
		case <-time.After(cmuxDownloadIdle):
			return
		}
	}
}
