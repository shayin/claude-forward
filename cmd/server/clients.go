package main

import (
	"net/http"

	"github.com/shayin/claude-forward/internal/server"
)

func clientsHandler(hub *server.Hub, auth *server.Auth) http.HandlerFunc {
	return auth.Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(hub.ListClients()))
	})
}
