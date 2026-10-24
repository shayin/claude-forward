package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shayin/claude-forward/internal/server"
)

func TestTerminalStaticFiles(t *testing.T) {
	tests := []struct {
		path    string
		noStore bool
	}{
		{"/cmux.html", true},
		{"/cmux.mjs?v=wechat-cache", true},
		{"/cmux-viewport.mjs", true},
		{"/cmux.css", true},
		{"/lib/xterm.min.js", true},
		{"/lib/xterm-addon-fit.min.js", true},
		{"/lib/xterm.css", true},
		{"/lib/marked/marked.esm.js", true},
		{"/index.html", false},
		{"/cmux-download", false},
		{"/other.mjs", false},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := terminalStaticFiles(next)
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			request.Header.Set("If-Modified-Since", "Sat, 24 Oct 2026 05:36:04 GMT")
			request.Header.Set("If-None-Match", `"old"`)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if tt.noStore {
				if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("terminal asset must return fresh content with no-store: code=%d headers=%v",
						recorder.Code, recorder.Header())
				}
			} else if recorder.Code != http.StatusNotModified ||
				strings.Contains(recorder.Header().Get("Cache-Control"), "no-store") {
				t.Fatalf("unrelated route must retain its cache behavior: code=%d headers=%v",
					recorder.Code, recorder.Header())
			}
		})
	}
}

func TestProtectTerminalAssetsRequiresBrowserSession(t *testing.T) {
	auth := server.NewAuth([]string{"valid-token"})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := protectTerminalAssets(auth, next)

	html := httptest.NewRequest(http.MethodGet, "/cmux.html", nil)
	htmlRecorder := httptest.NewRecorder()
	handler.ServeHTTP(htmlRecorder, html)
	if htmlRecorder.Code != http.StatusSeeOther ||
		htmlRecorder.Header().Get("Location") != "/?login_required=1" {
		t.Fatalf("unprotected terminal page: code=%d location=%q",
			htmlRecorder.Code, htmlRecorder.Header().Get("Location"))
	}

	module := httptest.NewRequest(http.MethodGet, "/cmux-scroll.mjs", nil)
	moduleRecorder := httptest.NewRecorder()
	handler.ServeHTTP(moduleRecorder, module)
	if moduleRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected terminal module: code=%d", moduleRecorder.Code)
	}

	public := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	publicRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publicRecorder, public)
	if publicRecorder.Code != http.StatusOK {
		t.Fatalf("login page must remain public: code=%d", publicRecorder.Code)
	}

	value, _, ok := auth.CreateBrowserSession("valid-token")
	if !ok {
		t.Fatal("cannot create browser session")
	}
	authorized := httptest.NewRequest(http.MethodGet, "/cmux.html", nil)
	authorized.AddCookie(&http.Cookie{Name: server.BrowserSessionCookie, Value: value})
	authorizedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(authorizedRecorder, authorized)
	if authorizedRecorder.Code != http.StatusOK {
		t.Fatalf("authorized terminal page code=%d", authorizedRecorder.Code)
	}
}
