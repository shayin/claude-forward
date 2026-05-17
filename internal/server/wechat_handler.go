package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// WeChatHandler 微信管理 HTTP API
type WeChatHandler struct {
	mgr  *WeChatManager
	auth *Auth
}

// NewWeChatHandler 创建微信管理 API 处理器
func NewWeChatHandler(mgr *WeChatManager, auth *Auth) *WeChatHandler {
	return &WeChatHandler{mgr: mgr, auth: auth}
}

// HandleStatus 返回所有微信用户状态
// GET /api/wechat/status
func (h *WeChatHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization")
		return
	}

	if !h.authenticate(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	statuses := h.mgr.GetStatus()
	json.NewEncoder(w).Encode(statuses)
}

// HandleQRCode 触发或获取二维码
// GET /api/wechat/qrcode/{index}
func (h *WeChatHandler) HandleQRCode(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization")
		return
	}

	if !h.authenticate(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 提取 index: /api/wechat/qrcode/0 → "0"
	path := strings.TrimPrefix(r.URL.Path, "/api/wechat/qrcode/")
	if path == "" {
		http.Error(w, "missing user index", http.StatusBadRequest)
		return
	}

	qrResult, err := h.mgr.StartQRLogin(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to start QR login: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	json.NewEncoder(w).Encode(map[string]string{
		"qr_code_url": qrResult.QRCodeURL,
		"token":       qrResult.Token,
	})
}

// HandleRelogin 强制重新登录
// POST /api/wechat/relogin/{index}
func (h *WeChatHandler) HandleRelogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		return
	}

	if !h.authenticate(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/wechat/relogin/")
	if path == "" {
		http.Error(w, "missing user index", http.StatusBadRequest)
		return
	}

	qrResult, err := h.mgr.StartQRLogin(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to start relogin: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	json.NewEncoder(w).Encode(map[string]string{
		"qr_code_url": qrResult.QRCodeURL,
		"token":       qrResult.Token,
	})
}

func (h *WeChatHandler) authenticate(r *http.Request) bool {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return h.auth.ValidateToken(token)
}

// pushRequest Push API 请求
type pushRequest struct {
	WechatID string `json:"wechat_id"`
	Text     string `json:"text"`
}

// HandlePush 处理推送消息请求
// POST /api/wechat/push
func (h *WeChatHandler) HandlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 验证 push_secret
	if !h.authenticatePush(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req pushRequest
	// 限制请求体大小为 64KB
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.WechatID == "" || req.Text == "" {
		http.Error(w, "wechat_id and text are required", http.StatusBadRequest)
		return
	}

	log.Printf("[PUSH] 收到推送请求 wechat_id=%s text_len=%d", req.WechatID, len(req.Text))

	// 白名单检查
	if !h.mgr.IsWechatIDInConfig(req.WechatID) {
		log.Printf("[PUSH] 白名单拒绝 wechat_id=%s", req.WechatID)
		http.Error(w, "wechat_id not allowed", http.StatusForbidden)
		return
	}

	status, err := h.mgr.PushMessage(req.WechatID, req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// authenticatePush 验证 Push API 密钥
func (h *WeChatHandler) authenticatePush(r *http.Request) bool {
	secret := h.mgr.config.PushSecret
	if secret == "" {
		return false
	}
	// 仅支持 Header 认证，避免 URL query 泄露到日志
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token == secret
}
