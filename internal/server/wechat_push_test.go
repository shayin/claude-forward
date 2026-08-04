package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestWechatManager() *WeChatManager {
	return NewWeChatManager(nil, nil, WeChatConfig{
		Enabled: true,
		Users: []UserRoute{
			{WechatID: "wxid_test@im.wechat", ClawbotID: "test-pc", PushSecret: "secret-aaa"},
			{WechatID: "wxid_test2@im.wechat", ClawbotID: "test-pc2", PushSecret: "secret-bbb"},
			{WechatID: "wxid_nosecret@im.wechat", ClawbotID: "test-pc3", PushSecret: ""},
		},
	})
}

// TestIsWechatIDInConfig 白名单检查 + 返回 push_secret
func TestIsWechatIDInConfig(t *testing.T) {
	mgr := newTestWechatManager()

	tests := []struct {
		wechatID       string
		wantInConfig   bool
		wantPushSecret string
	}{
		{"wxid_test@im.wechat", true, "secret-aaa"},
		{"wxid_test2@im.wechat", true, "secret-bbb"},
		{"wxid_nosecret@im.wechat", true, ""},
		{"wxid_unknown@im.wechat", false, ""},
		{"", false, ""},
	}

	for _, tt := range tests {
		inConfig, secret := mgr.IsWechatIDInConfig(tt.wechatID)
		if inConfig != tt.wantInConfig {
			t.Errorf("IsWechatIDInConfig(%q) inConfig = %v, want %v", tt.wechatID, inConfig, tt.wantInConfig)
		}
		if secret != tt.wantPushSecret {
			t.Errorf("IsWechatIDInConfig(%q) secret = %q, want %q", tt.wechatID, secret, tt.wantPushSecret)
		}
	}
}

// TestPushMessage_OfflineQueued 未登录时入队
func TestPushMessage_OfflineQueued(t *testing.T) {
	mgr := newTestWechatManager()

	status, err := mgr.PushMessage("wxid_test@im.wechat", "hello world")
	if err != nil {
		t.Fatalf("PushMessage error: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want %q", status, "queued")
	}

	mgr.mu.Lock()
	user := mgr.users["0"]
	queueLen := len(user.PushQueue)
	mgr.mu.Unlock()

	if queueLen != 1 {
		t.Fatalf("queue length = %d, want 1", queueLen)
	}

	item := user.PushQueue[0]
	if item.Text != "hello world" {
		t.Errorf("queued text = %q, want %q", item.Text, "hello world")
	}
	if item.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}

// TestPushMessage_NotInConfig 不在白名单返回错误
func TestPushMessage_NotInConfig(t *testing.T) {
	mgr := newTestWechatManager()

	_, err := mgr.PushMessage("wxid_unknown@im.wechat", "hello")
	if err == nil {
		t.Fatal("expected error for unknown wechat_id")
	}
}

// TestPushMessage_MultipleQueued 多条消息入队
func TestPushMessage_MultipleQueued(t *testing.T) {
	mgr := newTestWechatManager()

	for i := 0; i < 3; i++ {
		status, err := mgr.PushMessage("wxid_test@im.wechat", "msg"+strings.Repeat("", i+1))
		if err != nil {
			t.Fatalf("PushMessage[%d] error: %v", i, err)
		}
		if status != "queued" {
			t.Errorf("status[%d] = %q, want %q", i, status, "queued")
		}
	}

	mgr.mu.Lock()
	queueLen := len(mgr.users["0"].PushQueue)
	mgr.mu.Unlock()

	if queueLen != 3 {
		t.Errorf("queue length = %d, want 3", queueLen)
	}
}

// TestPushQueuePersistence 队列持久化
func TestPushQueuePersistence(t *testing.T) {
	mgr := newTestWechatManager()
	mgr.dataDir = t.TempDir()

	mgr.mu.Lock()
	user := mgr.users["0"]
	user.PushQueue = []pushQueueItem{
		{Text: "persisted msg", CreatedAt: time.Now()},
	}
	mgr.mu.Unlock()

	mgr.savePushQueue("0", user)

	user.PushQueue = nil
	mgr.loadPushQueue("0", user)

	if len(user.PushQueue) != 1 {
		t.Fatalf("queue length after reload = %d, want 1", len(user.PushQueue))
	}
	if user.PushQueue[0].Text != "persisted msg" {
		t.Errorf("text = %q, want %q", user.PushQueue[0].Text, "persisted msg")
	}
}

// TestPushQueueFlushClears 空队列 flush 无副作用
func TestPushQueueFlushClears(t *testing.T) {
	mgr := newTestWechatManager()

	mgr.mu.Lock()
	user := mgr.users["0"]
	user.PushQueue = nil
	mgr.mu.Unlock()

	mgr.flushPushQueue("0", user)

	if len(user.PushQueue) != 0 {
		t.Errorf("queue should be empty, got %d items", len(user.PushQueue))
	}
}

// TestFilterStalePushQueue 离线队列 TTL 过滤
func TestFilterStalePushQueue(t *testing.T) {
	now := time.Now()
	q := []pushQueueItem{
		{Text: "ancient", CreatedAt: now.Add(-30 * 24 * time.Hour)}, // 30 天前，过时
		{Text: "old", CreatedAt: now.Add(-2 * 24 * time.Hour)},      // 2 天前，过时
		{Text: "fresh", CreatedAt: now.Add(-1 * time.Hour)},         // 1 小时前，保留
		{Text: "no-ts", CreatedAt: time.Time{}},                     // 零值时间戳，保守保留
	}
	fresh, dropped := filterStalePushQueue(q, now, 24*time.Hour)
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if len(fresh) != 2 {
		t.Fatalf("fresh len = %d, want 2", len(fresh))
	}
	if fresh[0].Text != "fresh" {
		t.Errorf("fresh[0] = %q, want %q", fresh[0].Text, "fresh")
	}
	if fresh[1].Text != "no-ts" {
		t.Errorf("fresh[1] = %q, want %q", fresh[1].Text, "no-ts")
	}
}

// TestFlushPushQueue_DropsStaleMessages 全过时队列 flush 后清空（且不触发 Bot 投递）
func TestFlushPushQueue_DropsStaleMessages(t *testing.T) {
	mgr := newTestWechatManager()
	mgr.dataDir = t.TempDir()
	mgr.mu.Lock()
	user := mgr.users["0"]
	user.PushQueue = []pushQueueItem{
		{Text: "stale1", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)},
		{Text: "stale2", CreatedAt: time.Now().Add(-5 * 24 * time.Hour)},
	}
	mgr.mu.Unlock()

	mgr.flushPushQueue("0", user)

	if len(user.PushQueue) != 0 {
		t.Errorf("queue should be empty after dropping stale, got %d", len(user.PushQueue))
	}
}

// --- HTTP Handler 测试 ---

// TestHandlePush_PerUserAuth 用户级认证
func TestHandlePush_PerUserAuth(t *testing.T) {
	mgr := newTestWechatManager()
	handler := NewWeChatHandler(mgr, nil)

	tests := []struct {
		name      string
		wechatID  string
		secret    string
		want      int
	}{
		{"user-a correct secret", "wxid_test@im.wechat", "secret-aaa", http.StatusOK},
		{"user-a wrong secret", "wxid_test@im.wechat", "secret-bbb", http.StatusUnauthorized},
		{"user-b correct secret", "wxid_test2@im.wechat", "secret-bbb", http.StatusOK},
		{"user-a empty secret", "wxid_test@im.wechat", "", http.StatusUnauthorized},
		{"no secret user", "wxid_nosecret@im.wechat", "any-secret", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"wechat_id":"` + tt.wechatID + `","text":"hello"}`
			req := httptest.NewRequest(http.MethodPost, "/api/wechat/push", strings.NewReader(body))
			if tt.secret != "" {
				req.Header.Set("Authorization", "Bearer "+tt.secret)
			}
			w := httptest.NewRecorder()

			handler.HandlePush(w, req)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d, body=%s", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

// TestHandlePush_Whitelist 白名单检查
func TestHandlePush_Whitelist(t *testing.T) {
	mgr := newTestWechatManager()
	handler := NewWeChatHandler(mgr, nil)

	body := `{"wechat_id":"wxid_unknown@im.wechat","text":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/wechat/push", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-aaa")
	w := httptest.NewRecorder()

	handler.HandlePush(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestHandlePush_MissingParams 参数校验
func TestHandlePush_MissingParams(t *testing.T) {
	mgr := newTestWechatManager()
	handler := NewWeChatHandler(mgr, nil)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"missing text", `{"wechat_id":"wxid_test@im.wechat"}`, http.StatusBadRequest},
		{"missing wechat_id", `{"text":"hello"}`, http.StatusBadRequest},
		{"empty body", `{}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/wechat/push", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer secret-aaa")
			w := httptest.NewRecorder()

			handler.HandlePush(w, req)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// TestHandlePush_Success 推送成功响应（离线入队）
func TestHandlePush_Success(t *testing.T) {
	mgr := newTestWechatManager()
	handler := NewWeChatHandler(mgr, nil)

	body := `{"wechat_id":"wxid_test@im.wechat","text":"hello push"}`
	req := httptest.NewRequest(http.MethodPost, "/api/wechat/push", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-aaa")
	w := httptest.NewRecorder()

	handler.HandlePush(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "queued" {
		t.Errorf("status = %q, want %q", resp["status"], "queued")
	}
}

// TestHandlePush_MethodNotAllowed 非 POST 方法
func TestHandlePush_MethodNotAllowed(t *testing.T) {
	mgr := newTestWechatManager()
	handler := NewWeChatHandler(mgr, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/wechat/push", nil)
	w := httptest.NewRecorder()

	handler.HandlePush(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestHandlePush_InvalidJSON 无效 JSON
func TestHandlePush_InvalidJSON(t *testing.T) {
	mgr := newTestWechatManager()
	handler := NewWeChatHandler(mgr, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/wechat/push", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer secret-aaa")
	w := httptest.NewRecorder()

	handler.HandlePush(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
