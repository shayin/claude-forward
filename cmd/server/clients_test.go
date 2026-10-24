package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shayin/claude-forward/internal/server"
)

func TestClientsHandlerRequiresTokenAndDoesNotAllowCrossOriginReads(t *testing.T) {
	handler := clientsHandler(server.NewHub(), server.NewAuth([]string{"valid-token"}))
	tests := []struct {
		name   string
		method string
		token  string
		want   int
	}{
		{"missing token", http.MethodGet, "", http.StatusUnauthorized},
		{"invalid token", http.MethodGet, "Bearer invalid", http.StatusUnauthorized},
		{"valid token", http.MethodGet, "Bearer valid-token", http.StatusOK},
		{"unauthorized preflight", http.MethodOptions, "", http.StatusUnauthorized},
		{"unsupported method", http.MethodPost, "Bearer valid-token", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, "/api/clients", nil)
			request.Header.Set("Origin", "https://untrusted.example")
			if tt.token != "" {
				request.Header.Set("Authorization", tt.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tt.want {
				t.Fatalf("code = %d, want %d", recorder.Code, tt.want)
			}
			if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("client metadata must not be readable cross-origin")
			}
			if tt.want == http.StatusUnauthorized && recorder.Body.String() != "Unauthorized\n" {
				t.Fatalf("unauthorized response leaked data: %q", recorder.Body.String())
			}
			if tt.want == http.StatusOK {
				if recorder.Header().Get("Cache-Control") != "no-store" ||
					recorder.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("sensitive response headers missing: %v", recorder.Header())
				}
				if recorder.Body.String() != "null" {
					t.Fatalf("unexpected clients response: %q", recorder.Body.String())
				}
			}
		})
	}
}
