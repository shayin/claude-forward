package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	browserSessionBytes  = 32
	loginFailureWindow   = 5 * time.Minute
	loginFailureLimit    = 8
	maxBrowserSessions   = 256
	maxTokenSessions     = 16
	maxLoginFailureHosts = 1024
)

type loginFailures struct {
	count   int
	started time.Time
}

type BrowserSessionHandler struct {
	auth     *Auth
	mu       sync.Mutex
	failures map[string]loginFailures
}

func NewBrowserSessionHandler(auth *Auth) *BrowserSessionHandler {
	return &BrowserSessionHandler{
		auth: auth, failures: make(map[string]loginFailures),
	}
}

func (a *Auth) CreateBrowserSession(token string) (string, time.Time, bool) {
	if !a.ValidateToken(token) {
		return "", time.Time{}, false
	}
	raw := make([]byte, browserSessionBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, false
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	key := sha256.Sum256([]byte(value))
	principal := tokenPrincipal(token)
	expiresAt := a.now().Add(browserSessionTTL)
	a.mu.Lock()
	a.removeExpiredSessionsLocked()
	a.trimBrowserSessionsLocked(principal)
	a.sessions[key] = browserSession{principal: principal, expiresAt: expiresAt}
	a.mu.Unlock()
	return value, expiresAt, true
}

func (a *Auth) browserSessionDetails(r *http.Request) ([sha256.Size]byte, browserSession, bool) {
	cookie, err := r.Cookie(BrowserSessionCookie)
	if err != nil || cookie.Value == "" {
		return [sha256.Size]byte{}, browserSession{}, false
	}
	key := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	if !ok || !session.expiresAt.After(a.now()) {
		delete(a.sessions, key)
		return [sha256.Size]byte{}, browserSession{}, false
	}
	return key, session, true
}

func (a *Auth) browserSession(r *http.Request) (browserSession, bool) {
	_, session, ok := a.browserSessionDetails(r)
	return session, ok
}

func (a *Auth) ValidateBrowserSession(r *http.Request) bool {
	_, ok := a.browserSession(r)
	return ok
}

func (a *Auth) validateBrowserSessionKey(key [sha256.Size]byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	if !ok || !session.expiresAt.After(a.now()) {
		delete(a.sessions, key)
		return false
	}
	return true
}

func (a *Auth) RevokeBrowserSession(r *http.Request) {
	cookie, err := r.Cookie(BrowserSessionCookie)
	if err != nil || cookie.Value == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
	a.mu.Unlock()
}

func (a *Auth) RequestPrincipal(r *http.Request) ([sha256.Size]byte, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	value = strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	if a.ValidateToken(value) {
		return tokenPrincipal(value), true
	}
	if session, ok := a.browserSession(r); ok {
		return session.principal, true
	}
	return [sha256.Size]byte{}, false
}

func (a *Auth) removeExpiredSessionsLocked() {
	now := a.now()
	for key, session := range a.sessions {
		if !session.expiresAt.After(now) {
			delete(a.sessions, key)
		}
	}
}

func (a *Auth) trimBrowserSessionsLocked(principal [sha256.Size]byte) {
	removeOldest := func(match func(browserSession) bool) bool {
		var oldestKey [sha256.Size]byte
		var oldest time.Time
		found := false
		for key, session := range a.sessions {
			if match(session) && (!found || session.expiresAt.Before(oldest)) {
				oldestKey, oldest, found = key, session.expiresAt, true
			}
		}
		if found {
			delete(a.sessions, oldestKey)
		}
		return found
	}
	count := 0
	for _, session := range a.sessions {
		if session.principal == principal {
			count++
		}
	}
	for count >= maxTokenSessions &&
		removeOldest(func(session browserSession) bool { return session.principal == principal }) {
		count--
	}
	for len(a.sessions) >= maxBrowserSessions &&
		removeOldest(func(browserSession) bool { return true }) {
	}
}

func sameBrowserOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return parsed.Scheme == scheme && strings.EqualFold(parsed.Host, r.Host)
}

func browserSessionCookie(value string, expires time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name: BrowserSessionCookie, Value: value, Path: "/",
		Expires: expires, MaxAge: int(browserSessionTTL.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	}
}

func clearBrowserSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name: BrowserSessionCookie, Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func browserClientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func browserSessionHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (h *BrowserSessionHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	browserSessionHeaders(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameBrowserOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	address := browserClientAddress(r)
	if !h.loginAllowed(address) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1)
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) != 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	value, expiresAt, ok := h.auth.CreateBrowserSession(token)
	if !ok {
		h.recordLoginFailure(address)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	h.clearLoginFailures(address)
	http.SetCookie(w, browserSessionCookie(value, expiresAt, r.TLS != nil))
	w.WriteHeader(http.StatusNoContent)
}

func (h *BrowserSessionHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	browserSessionHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.auth.ValidateBrowserSession(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *BrowserSessionHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	browserSessionHeaders(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameBrowserOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	h.auth.RevokeBrowserSession(r)
	http.SetCookie(w, clearBrowserSessionCookie(r.TLS != nil))
	w.WriteHeader(http.StatusNoContent)
}

func (h *BrowserSessionHandler) loginAllowed(address string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.failures[address]
	if !ok || h.auth.now().Sub(entry.started) >= loginFailureWindow {
		delete(h.failures, address)
		return true
	}
	return entry.count < loginFailureLimit
}

func (h *BrowserSessionHandler) recordLoginFailure(address string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.auth.now()
	if len(h.failures) >= maxLoginFailureHosts {
		for key, candidate := range h.failures {
			if now.Sub(candidate.started) >= loginFailureWindow {
				delete(h.failures, key)
			}
		}
		for len(h.failures) >= maxLoginFailureHosts {
			var oldestKey string
			var oldest time.Time
			for key, candidate := range h.failures {
				if oldestKey == "" || candidate.started.Before(oldest) {
					oldestKey, oldest = key, candidate.started
				}
			}
			delete(h.failures, oldestKey)
		}
	}
	entry := h.failures[address]
	if entry.started.IsZero() || now.Sub(entry.started) >= loginFailureWindow {
		entry = loginFailures{started: now}
	}
	entry.count++
	h.failures[address] = entry
}

func (h *BrowserSessionHandler) clearLoginFailures(address string) {
	h.mu.Lock()
	delete(h.failures, address)
	h.mu.Unlock()
}
