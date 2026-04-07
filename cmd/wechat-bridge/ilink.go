package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// iLink Bot API 常量
const (
	ilinkAppID          = "bot"
	ilinkChannelVersion = "2.1.7"
	defaultBaseURL      = "https://ilinkai.weixin.qq.com"
)

// iLinkBot 封装 iLink Bot API
type iLinkBot struct {
	BaseURL   string
	Token     string
	Client    *http.Client
	UpdateBuf string
}

// newILinkBot 创建 iLink Bot 客户端
func newILinkBot() *iLinkBot {
	return &iLinkBot{
		BaseURL: defaultBaseURL,
		Client: &http.Client{
			Timeout: 40 * time.Second, // 长轮询需要较长超时
		},
	}
}

// --- Login ---

// qrCodeResponse 获取二维码响应
type qrCodeResponse struct {
	QRCode string `json:"qrcode"`
}

// qrStatusResponse 二维码状态响应
type qrStatusResponse struct {
	Status      string `json:"status"`       // wait, scaned, confirmed, expired
	BotToken    string `json:"bot_token"`
	ILinkBotID  string `json:"ilink_bot_id"`
	BaseURL     string `json:"baseurl"`
	ILinkUserID string `json:"ilink_user_id"`
}

// LoginResult 登录结果
type LoginResult struct {
	BotToken string
	BotID    string
	BaseURL  string
	UserID   string
}

// fetchQRCode 获取登录二维码
func (b *iLinkBot) fetchQRCode() (string, error) {
	url := fmt.Sprintf("%s/ilink/bot/get_bot_qrcode?bot_type=3", b.BaseURL)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("iLink-App-Id", ilinkAppID)
	req.Header.Set("iLink-App-ClientVersion", "131143") // 2.1.7 encoded

	resp, err := b.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch QR code failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch QR code failed: %s", body)
	}

	var result qrCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse QR code response failed: %w", err)
	}

	return result.QRCode, nil
}

// waitForLogin 等待扫码登录
func (b *iLinkBot) waitForLogin(qrcode string, timeout time.Duration) (*LoginResult, error) {
	deadline := time.Now().Add(timeout)
	pollClient := &http.Client{Timeout: 40 * time.Second}

	for time.Now().Before(deadline) {
		url := fmt.Sprintf("%s/ilink/bot/get_qrcode_status?qrcode=%s", b.BaseURL, qrcode)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("iLink-App-Id", ilinkAppID)
		req.Header.Set("iLink-App-ClientVersion", "131143")

		resp, err := pollClient.Do(req)
		if err != nil {
			log.Printf("Poll QR status error: %v, retrying...", err)
			time.Sleep(3 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var status qrStatusResponse
		if err := json.Unmarshal(body, &status); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		switch status.Status {
		case "confirmed":
			if status.BotToken == "" {
				return nil, fmt.Errorf("login confirmed but no bot_token received")
			}
			baseURL := status.BaseURL
			if baseURL == "" {
				baseURL = b.BaseURL
			}
			return &LoginResult{
				BotToken: status.BotToken,
				BotID:    status.ILinkBotID,
				BaseURL:  baseURL,
				UserID:   status.ILinkUserID,
			}, nil
		case "expired":
			return nil, fmt.Errorf("QR code expired")
		case "scaned":
			log.Println("QR code scanned, waiting for confirmation...")
		case "wait":
			// continue polling
		}
	}

	return nil, fmt.Errorf("login timeout")
}

// --- Message API ---

// weixinMessage 微信消息
type weixinMessage struct {
	Seq          int           `json:"seq,omitempty"`
	MessageID    int           `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
	ItemList     []messageItem `json:"item_list,omitempty"`
}

type messageItem struct {
	Type     int       `json:"type,omitempty"`
	TextItem *textItem `json:"text_item,omitempty"`
}

type textItem struct {
	Text string `json:"text,omitempty"`
}

// getUpdatesResponse getupdates 响应
type getUpdatesResponse struct {
	Ret           int             `json:"ret"`
	ErrCode       int             `json:"errcode,omitempty"`
	ErrMsg        string          `json:"errmsg,omitempty"`
	Msgs          []weixinMessage `json:"msgs,omitempty"`
	GetUpdatesBuf string          `json:"get_updates_buf,omitempty"`
}

// sendMessageRequest sendmessage 请求
type sendMessageRequest struct {
	Msg      weixinMessage `json:"msg"`
	BaseInfo baseInfo      `json:"base_info"`
}

type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

// getConfigResponse getconfig 响应
type getConfigResponse struct {
	Ret          int    `json:"ret"`
	TypingTicket string `json:"typing_ticket,omitempty"`
}

// randomWechatUin 生成随机 X-WECHAT-UIN
func randomWechatUin() string {
	buf := make([]byte, 4)
	rand.Read(buf)
	uint32 := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", uint32)))
}

// buildHeaders 构建请求头
func (b *iLinkBot) buildHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("X-WECHAT-UIN", randomWechatUin())
	h.Set("iLink-App-Id", ilinkAppID)
	h.Set("iLink-App-ClientVersion", "131143")
	if b.Token != "" {
		h.Set("Authorization", "Bearer "+b.Token)
	}
	return h
}

// post 发送 POST 请求
func (b *iLinkBot) post(endpoint string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/ilink/bot/%s", b.BaseURL, endpoint)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = b.buildHeaders()

	// getUpdates 使用长超时
	client := b.Client
	if endpoint == "getupdates" {
		client = &http.Client{Timeout: 40 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s failed: %s", endpoint, respBody)
	}

	return respBody, nil
}

// GetUpdates 长轮询获取消息
func (b *iLinkBot) GetUpdates() ([]weixinMessage, string, error) {
	payload := map[string]any{
		"get_updates_buf": b.UpdateBuf,
		"base_info":       baseInfo{ChannelVersion: ilinkChannelVersion},
	}

	respBody, err := b.post("getupdates", payload)
	if err != nil {
		return nil, b.UpdateBuf, err
	}

	var resp getUpdatesResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, b.UpdateBuf, err
	}

	if resp.Ret != 0 {
		return nil, b.UpdateBuf, fmt.Errorf("getupdates returned ret=%d, errcode=%d, errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}

	// 更新 buf
	newBuf := resp.GetUpdatesBuf
	if newBuf != "" {
		b.UpdateBuf = newBuf
	}

	return resp.Msgs, b.UpdateBuf, nil
}

// SendMessage 发送文本消息
func (b *iLinkBot) SendMessage(toUserID, text, contextToken string) error {
	clientID := fmt.Sprintf("openclaw-weixin-%x", make([]byte, 8))
	rand.Read([]byte(clientID[len("openclaw-weixin-"):]))

	msg := weixinMessage{
		FromUserID:  "",
		ToUserID:    toUserID,
		ClientID:    clientID,
		MessageType: 2, // BOT
		MessageState: 2, // FINISH
		ContextToken: contextToken,
		ItemList: []messageItem{
			{Type: 1, TextItem: &textItem{Text: text}},
		},
	}

	_, err := b.post("sendmessage", sendMessageRequest{
		Msg:      msg,
		BaseInfo: baseInfo{ChannelVersion: ilinkChannelVersion},
	})
	return err
}

// SendTyping 发送输入状态
func (b *iLinkBot) SendTyping(userID string, ticket string, status int) error {
	payload := map[string]any{
		"ilink_user_id": userID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info":     baseInfo{ChannelVersion: ilinkChannelVersion},
	}
	_, err := b.post("sendtyping", payload)
	return err
}

// GetConfig 获取配置（包含 typing_ticket）
func (b *iLinkBot) GetConfig(userID string) (string, error) {
	payload := map[string]any{
		"ilink_user_id": userID,
		"base_info":     baseInfo{ChannelVersion: ilinkChannelVersion},
	}

	respBody, err := b.post("getconfig", payload)
	if err != nil {
		return "", err
	}

	var resp getConfigResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", err
	}

	return resp.TypingTicket, nil
}
