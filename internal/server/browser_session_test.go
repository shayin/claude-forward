package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func loginBrowser(t *testing.T, handler *BrowserSessionHandler, token string) *http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://example.com/api/session/login", nil)
	request.Host = "example.com"
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.HandleLogin(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("login code = %d body = %q", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	return cookies[0]
}

func TestBrowserSessionLoginStatusLogoutAndCookieAttributes(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	handler := NewBrowserSessionHandler(auth)
	cookie := loginBrowser(t, handler, "valid-token")
	if cookie.Name != BrowserSessionCookie || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.Secure ||
		cookie.Path != "/" || cookie.MaxAge <= 0 {
		t.Fatalf("unsafe cookie attributes: %+v", cookie)
	}

	status := httptest.NewRequest(http.MethodGet, "http://example.com/api/session/status", nil)
	status.AddCookie(cookie)
	statusRecorder := httptest.NewRecorder()
	handler.HandleStatus(statusRecorder, status)
	if statusRecorder.Code != http.StatusNoContent {
		t.Fatalf("status code = %d", statusRecorder.Code)
	}

	logout := httptest.NewRequest(http.MethodPost, "http://example.com/api/session/logout", nil)
	logout.Host = "example.com"
	logout.Header.Set("Origin", "http://example.com")
	logout.AddCookie(cookie)
	logoutRecorder := httptest.NewRecorder()
	handler.HandleLogout(logoutRecorder, logout)
	if logoutRecorder.Code != http.StatusNoContent ||
		len(logoutRecorder.Result().Cookies()) != 1 ||
		logoutRecorder.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout code=%d cookies=%v", logoutRecorder.Code,
			logoutRecorder.Result().Cookies())
	}

	statusRecorder = httptest.NewRecorder()
	handler.HandleStatus(statusRecorder, status)
	if statusRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session code = %d", statusRecorder.Code)
	}
}

func TestBrowserSessionRejectsInvalidLoginAndCrossSiteRequests(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	handler := NewBrowserSessionHandler(auth)
	tests := []struct {
		name   string
		origin string
		token  string
		want   int
	}{
		{"missing origin", "", "valid-token", http.StatusForbidden},
		{"cross site", "https://evil.example", "valid-token", http.StatusForbidden},
		{"wrong scheme", "https://example.com", "valid-token", http.StatusForbidden},
		{"invalid token", "http://example.com", "invalid", http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost,
				"http://example.com/api/session/login", nil)
			request.Host = "example.com"
			request.RemoteAddr = "192.0.2.20:1234"
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			request.Header.Set("Authorization", "Bearer "+test.token)
			recorder := httptest.NewRecorder()
			handler.HandleLogin(recorder, request)
			if recorder.Code != test.want || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("code=%d headers=%v", recorder.Code, recorder.Header())
			}
		})
	}
}

func TestBrowserSessionLoginRejectsRequestBody(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	handler := NewBrowserSessionHandler(auth)
	request := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/session/login", strings.NewReader("unexpected"))
	request.Host = "example.com"
	request.RemoteAddr = "192.0.2.25:1234"
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Authorization", "Bearer valid-token")
	recorder := httptest.NewRecorder()
	handler.HandleLogin(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("body accepted with code %d", recorder.Code)
	}
}

func TestBrowserSessionLoginRateLimitExpires(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	auth.now = func() time.Time { return now }
	handler := NewBrowserSessionHandler(auth)
	for attempt := 0; attempt < loginFailureLimit; attempt++ {
		request := httptest.NewRequest(http.MethodPost,
			"http://example.com/api/session/login", nil)
		request.Host = "example.com"
		request.RemoteAddr = "192.0.2.30:1234"
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Authorization", "Bearer invalid")
		recorder := httptest.NewRecorder()
		handler.HandleLogin(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d code = %d", attempt, recorder.Code)
		}
	}
	blocked := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/session/login", nil)
	blocked.Host = "example.com"
	blocked.RemoteAddr = "192.0.2.30:1234"
	blocked.Header.Set("Origin", "http://example.com")
	blocked.Header.Set("Authorization", "Bearer valid-token")
	blockedRecorder := httptest.NewRecorder()
	handler.HandleLogin(blockedRecorder, blocked)
	if blockedRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked code = %d", blockedRecorder.Code)
	}
	now = now.Add(loginFailureWindow)
	recoveredRecorder := httptest.NewRecorder()
	handler.HandleLogin(recoveredRecorder, blocked)
	if recoveredRecorder.Code != http.StatusNoContent {
		t.Fatalf("recovered code = %d", recoveredRecorder.Code)
	}
}

func TestBrowserSessionsAreBoundedPerToken(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	for index := 0; index < maxTokenSessions+5; index++ {
		if _, _, ok := auth.CreateBrowserSession("valid-token"); !ok {
			t.Fatalf("session %d was not created", index)
		}
	}
	auth.mu.Lock()
	count := len(auth.sessions)
	auth.mu.Unlock()
	if count != maxTokenSessions {
		t.Fatalf("sessions = %d, want %d", count, maxTokenSessions)
	}
}

func TestHTTPSBrowserSessionCookieIsSecure(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	handler := NewBrowserSessionHandler(auth)
	request := httptest.NewRequest(http.MethodPost,
		"https://example.com/api/session/login", nil)
	request.Host = "example.com"
	request.RemoteAddr = "192.0.2.40:1234"
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Authorization", "Bearer valid-token")
	recorder := httptest.NewRecorder()
	handler.HandleLogin(recorder, request)
	if recorder.Code != http.StatusNoContent || !recorder.Result().Cookies()[0].Secure {
		t.Fatalf("HTTPS cookie is not secure: %v", recorder.Result().Cookies())
	}
}

func TestWebSocketAuthenticationSeparatesBrowserAndMachineClients(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	sessionHandler := NewBrowserSessionHandler(auth)
	cookie := loginBrowser(t, sessionHandler, "valid-token")
	handler := NewHandler(NewHub(), auth, nil)
	tests := []struct {
		name   string
		origin string
		token  string
		cookie *http.Cookie
		want   int
	}{
		{"browser query token rejected", "http://example.com", "valid-token", nil, http.StatusUnauthorized},
		{"cross site browser rejected", "https://evil.example", "", cookie, http.StatusForbidden},
		{"machine token", "", "valid-token", nil, http.StatusBadRequest},
		{"machine missing token", "", "", nil, http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := "http://example.com/ws"
			if test.token != "" {
				target += "?token=" + test.token
			}
			request := httptest.NewRequest(http.MethodGet, target, nil)
			request.Host = "example.com"
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.cookie != nil {
				request.AddCookie(test.cookie)
			}
			recorder := httptest.NewRecorder()
			handler.HandleWS(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("code=%d want=%d body=%q",
					recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func TestWebSocketUpgradeAcceptsBrowserSessionAndMachineToken(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	sessionHandler := NewBrowserSessionHandler(auth)
	cookie := loginBrowser(t, sessionHandler, "valid-token")
	handler := NewHandler(NewHub(), auth, nil)
	server := httptest.NewServer(http.HandlerFunc(handler.HandleWS))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	browserHeader := http.Header{}
	browserHeader.Set("Origin", server.URL)
	browserHeader.Set("Cookie", cookie.String())
	browser, response, err := websocket.DefaultDialer.Dial(wsURL, browserHeader)
	if err != nil {
		t.Fatalf("browser websocket: %v response=%v", err, response)
	}
	browser.Close()

	machine, response, err := websocket.DefaultDialer.Dial(
		wsURL+"?token="+url.QueryEscape("valid-token"), nil)
	if err != nil {
		t.Fatalf("machine websocket: %v response=%v", err, response)
	}
	machine.Close()

	crossSiteHeader := http.Header{}
	crossSiteHeader.Set("Origin", "https://evil.example")
	crossSiteHeader.Set("Cookie", cookie.String())
	_, response, err = websocket.DefaultDialer.Dial(wsURL, crossSiteHeader)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site websocket err=%v response=%v", err, response)
	}
}

func TestBrowserSessionAuthorizesDownloadPreparationWithOriginCheck(t *testing.T) {
	auth := NewAuth([]string{"valid-token"})
	sessionHandler := NewBrowserSessionHandler(auth)
	cookie := loginBrowser(t, sessionHandler, "valid-token")
	handler := NewHandler(NewHub(), auth, nil)

	request := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/cmux-download",
		strings.NewReader(`{"name":"safe.txt","size":1}`))
	request.Host = "example.com"
	request.Header.Set("Origin", "http://example.com")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.HandleCmuxDownload(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("download prepare code=%d body=%q", recorder.Code, recorder.Body.String())
	}

	crossSite := httptest.NewRequest(http.MethodPost,
		"http://example.com/api/cmux-download",
		strings.NewReader(`{"name":"safe.txt","size":1}`))
	crossSite.Host = "example.com"
	crossSite.Header.Set("Origin", "https://evil.example")
	crossSite.AddCookie(cookie)
	crossSiteRecorder := httptest.NewRecorder()
	handler.HandleCmuxDownload(crossSiteRecorder, crossSite)
	if crossSiteRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-site download code=%d", crossSiteRecorder.Code)
	}
}
