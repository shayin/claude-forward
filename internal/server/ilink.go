package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

const (
	ilinkAppID          = "bot"
	ilinkChannelVersion = "2.1.7"
	ilinkDefaultBaseURL = "https://ilinkai.weixin.qq.com"
)

// ILinkBot 封装 iLink Bot API
type ILinkBot struct {
	BaseURL   string
	Token     string
	Client    *http.Client
	UpdateBuf string
}

// NewILinkBot 创建 iLink Bot 客户端
func NewILinkBot() *ILinkBot {
	return &ILinkBot{
		BaseURL: ilinkDefaultBaseURL,
		Client: &http.Client{
			Timeout: 40 * time.Second,
		},
	}
}

// --- 数据结构 ---

type ilinkQRCodeResponse struct {
	QRCode         string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type ilinkQRStatusResponse struct {
	Status      string `json:"status"`
	BotToken    string `json:"bot_token"`
	ILinkBotID  string `json:"ilink_bot_id"`
	BaseURL     string `json:"baseurl"`
	ILinkUserID string `json:"ilink_user_id"`
}

type ILinkLoginResult struct {
	BotToken string `json:"bot_token"`
	BotID    string `json:"bot_id"`
	BaseURL  string `json:"base_url"`
	UserID   string `json:"user_id"`
}

type ilinkMessage struct {
	Seq          int           `json:"seq,omitempty"`
	MessageID    int           `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
	ItemList     []ilinkItem   `json:"item_list,omitempty"`
}

type ilinkItem struct {
	Type     int          `json:"type,omitempty"`
	TextItem *ilinkText   `json:"text_item,omitempty"`
}

type ilinkText struct {
	Text string `json:"text,omitempty"`
}

type ilinkGetUpdatesResponse struct {
	Ret           int            `json:"ret"`
	ErrCode       int            `json:"errcode,omitempty"`
	ErrMsg        string         `json:"errmsg,omitempty"`
	Msgs          []ilinkMessage `json:"msgs,omitempty"`
	GetUpdatesBuf string         `json:"get_updates_buf,omitempty"`
}

type ilinkSendMessageRequest struct {
	Msg      ilinkMessage `json:"msg"`
	BaseInfo ilinkBaseInfo `json:"base_info"`
}

type ilinkBaseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

type ilinkSendMessageResponse struct {
	Ret     int    `json:"ret"`
	ErrCode int    `json:"errcode,omitempty"`
	ErrMsg  string `json:"errmsg,omitempty"`
}

type ilinkGetConfigResponse struct {
	Ret          int    `json:"ret"`
	TypingTicket string `json:"typing_ticket,omitempty"`
}

// --- QR 码登录 ---

type ILinkQRStartResult struct {
	Token     string
	QRCodeURL string
}

// FetchQRCode 获取登录二维码
func (b *ILinkBot) FetchQRCode() (*ILinkQRStartResult, error) {
	url := fmt.Sprintf("%s/ilink/bot/get_bot_qrcode?bot_type=3", b.BaseURL)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-Id", ilinkAppID)
	req.Header.Set("iLink-App-ClientVersion", "131143")

	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch QR code failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := readAll(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch QR code failed: %s", body)
	}

	var result ilinkQRCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse QR code response failed: %w", err)
	}

	return &ILinkQRStartResult{
		Token:     result.QRCode,
		QRCodeURL: result.QRCodeImgContent,
	}, nil
}

// WaitForLogin 等待扫码登录
func (b *ILinkBot) WaitForLogin(qrcode string, timeout time.Duration) (*ILinkLoginResult, error) {
	deadline := time.Now().Add(timeout)
	pollClient := &http.Client{Timeout: 40 * time.Second}

	for time.Now().Before(deadline) {
		url := fmt.Sprintf("%s/ilink/bot/get_qrcode_status?qrcode=%s", b.BaseURL, qrcode)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("iLink-App-Id", ilinkAppID)
		req.Header.Set("iLink-App-ClientVersion", "131143")

		resp, err := pollClient.Do(req)
		if err != nil {
			log.Printf("[ILINK] Poll QR status error: %v, retrying...", err)
			time.Sleep(3 * time.Second)
			continue
		}

		body, _ := readAll(resp)
		resp.Body.Close()

		var status ilinkQRStatusResponse
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
			return &ILinkLoginResult{
				BotToken: status.BotToken,
				BotID:    status.ILinkBotID,
				BaseURL:  baseURL,
				UserID:   status.ILinkUserID,
			}, nil
		case "expired":
			return nil, fmt.Errorf("QR code expired")
		case "scaned":
			log.Println("[ILINK] QR code scanned, waiting for confirmation...")
		case "wait":
			// continue polling
		}
	}

	return nil, fmt.Errorf("login timeout")
}

// ValidateSession 验证 token 是否有效
func (b *ILinkBot) ValidateSession() bool {
	payload := map[string]any{
		"get_updates_buf": "",
		"base_info":       ilinkBaseInfo{ChannelVersion: ilinkChannelVersion},
	}
	respBody, err := b.post("getupdates", payload)
	if err != nil {
		return false
	}
	var resp ilinkGetUpdatesResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return false
	}
	return resp.Ret == 0
}

// --- 消息 API ---

// ILinkIncomingMessage 收到的微信消息
type ILinkIncomingMessage struct {
	FromUserID   string
	Text         string
	ContextToken string
}

// GetUpdates 长轮询获取消息
func (b *ILinkBot) GetUpdates() ([]ILinkIncomingMessage, string, error) {
	payload := map[string]any{
		"get_updates_buf": b.UpdateBuf,
		"base_info":       ilinkBaseInfo{ChannelVersion: ilinkChannelVersion},
	}

	respBody, err := b.post("getupdates", payload)
	if err != nil {
		return nil, b.UpdateBuf, err
	}

	var resp ilinkGetUpdatesResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, b.UpdateBuf, err
	}

	if resp.Ret != 0 {
		return nil, b.UpdateBuf, fmt.Errorf("getupdates ret=%d, errcode=%d, errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}

	newBuf := resp.GetUpdatesBuf
	if newBuf != "" {
		b.UpdateBuf = newBuf
	}

	var msgs []ILinkIncomingMessage
	for _, m := range resp.Msgs {
		if m.MessageType != 1 || len(m.ItemList) == 0 {
			continue
		}
		var text string
		for _, item := range m.ItemList {
			if item.TextItem != nil {
				text += item.TextItem.Text
			}
		}
		if text == "" {
			continue
		}
		msgs = append(msgs, ILinkIncomingMessage{
			FromUserID:   m.FromUserID,
			Text:         text,
			ContextToken: m.ContextToken,
		})
	}

	return msgs, b.UpdateBuf, nil
}

// SendMessage 发送文本消息
func (b *ILinkBot) SendMessage(toUserID, text, contextToken string) error {
	randBytes := make([]byte, 8)
	rand.Read(randBytes)
	clientID := fmt.Sprintf("cf-weixin-%x", randBytes)

	msg := ilinkMessage{
		FromUserID:  "",
		ToUserID:    toUserID,
		ClientID:    clientID,
		MessageType: 2,
		MessageState: 2,
		ContextToken: contextToken,
		ItemList: []ilinkItem{
			{Type: 1, TextItem: &ilinkText{Text: text}},
		},
	}

	respBody, err := b.post("sendmessage", ilinkSendMessageRequest{
		Msg:      msg,
		BaseInfo: ilinkBaseInfo{ChannelVersion: ilinkChannelVersion},
	})
	if err != nil {
		return err
	}

	var resp ilinkSendMessageResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		log.Printf("[ILINK] SendMessage parse response failed: %v", err)
		return nil
	}
	if resp.Ret != 0 {
		return fmt.Errorf("sendmessage failed: ret=%d, errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	return nil
}

// SendTyping 发送输入状态
func (b *ILinkBot) SendTyping(userID string, ticket string, status int) error {
	payload := map[string]any{
		"ilink_user_id": userID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info":     ilinkBaseInfo{ChannelVersion: ilinkChannelVersion},
	}
	_, err := b.post("sendtyping", payload)
	return err
}

// GetConfig 获取配置（包含 typing_ticket）
func (b *ILinkBot) GetConfig(userID string) (string, error) {
	payload := map[string]any{
		"ilink_user_id": userID,
		"base_info":     ilinkBaseInfo{ChannelVersion: ilinkChannelVersion},
	}

	respBody, err := b.post("getconfig", payload)
	if err != nil {
		return "", err
	}

	var resp ilinkGetConfigResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", err
	}

	return resp.TypingTicket, nil
}

// --- 内部方法 ---

func randomWechatUin() string {
	buf := make([]byte, 4)
	rand.Read(buf)
	v := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", v)))
}

func (b *ILinkBot) buildHeaders() http.Header {
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

func (b *ILinkBot) post(endpoint string, payload any) ([]byte, error) {
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

	client := b.Client
	if endpoint == "getupdates" {
		client = &http.Client{Timeout: 40 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := readAll(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s failed: %s", endpoint, respBody)
	}

	return respBody, nil
}

func readAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}
