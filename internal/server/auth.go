package server

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	BrowserSessionCookie = "cf_session"
	browserSessionTTL    = 12 * time.Hour
)

type browserSession struct {
	principal [sha256.Size]byte
	expiresAt time.Time
}

// Auth 认证中间件
type Auth struct {
	tokens   map[string]bool
	mu       sync.Mutex
	sessions map[[sha256.Size]byte]browserSession
	now      func() time.Time
}

// NewAuth 创建认证实例
func NewAuth(tokens []string) *Auth {
	tokenMap := make(map[string]bool)
	for _, t := range tokens {
		tokenMap[t] = true
	}
	return &Auth{
		tokens:   tokenMap,
		sessions: make(map[[sha256.Size]byte]browserSession),
		now:      time.Now,
	}
}

// Middleware 返回认证中间件
func (a *Auth) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 从 Header 或 URL 参数获取 token
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		// Bearer token 格式
		if strings.HasPrefix(token, "Bearer ") {
			token = strings.TrimPrefix(token, "Bearer ")
		}

		if !a.ValidateToken(token) && !a.ValidateBrowserSession(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// ValidateToken 验证 token
func (a *Auth) ValidateToken(token string) bool {
	return a.tokens[token]
}

func tokenPrincipal(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}
