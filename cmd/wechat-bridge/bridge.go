package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

// Bridge Claude Forward Server Bot API 客户端
type Bridge struct {
	ServerURL string
	Token     string
	Client    *http.Client
	sessions  map[string]*userSession // wechatUserID → session
}

// userSession 微信用户的会话状态
type userSession struct {
	ClawbotID  string    // 当前绑定的 clawbot_id
	ClientID   string    // 当前绑定的 client_id
	LastActive time.Time // 最后活跃时间
}

// NewBridge 创建 Bridge 客户端
func NewBridge(serverURL, token string) *Bridge {
	return &Bridge{
		ServerURL: strings.TrimSuffix(serverURL, "/"),
		Token:     token,
		Client:    &http.Client{Timeout: 5 * time.Minute},
		sessions:  make(map[string]*userSession),
	}
}

// ChatResponse SSE 流返回的完整回复
type ChatResponse struct {
	FullText  string
	ToolCalls []string
	CostUSD   float64
	IsError   bool
	ErrorMsg  string
}

// chatRequest Bot API 请求
type chatRequest struct {
	ClawbotID string `json:"clawbot_id,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	Text      string `json:"text"`
}

// GetSession 获取用户会话
func (b *Bridge) GetSession(wechatUserID string) *userSession {
	return b.sessions[wechatUserID]
}

// SetSession 设置用户会话
func (b *Bridge) SetSession(wechatUserID, clawbotID, clientID string) {
	b.sessions[wechatUserID] = &userSession{
		ClawbotID:  clawbotID,
		ClientID:   clientID,
		LastActive: time.Now(),
	}
}

// Chat 发送消息并等待完整回复
// 优先使用会话中的 client_id，否则用 clawbot_id 自动解析
func (b *Bridge) Chat(wechatUserID, text string) (*ChatResponse, error) {
	session := b.sessions[wechatUserID]

	reqBody := chatRequest{Text: text}
	if session != nil && session.ClientID != "" {
		reqBody.ClientID = session.ClientID
	} else if session != nil && session.ClawbotID != "" {
		reqBody.ClawbotID = session.ClawbotID
	}

	body, _ := json.Marshal(reqBody)
	url := b.ServerURL + "/api/bot/chat"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bridge returned %d: %s", resp.StatusCode, respBody)
	}

	result := &ChatResponse{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var msg protocol.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue
		}

		switch msg.Type {
		case protocol.TypeChatMessage:
			var payload protocol.ChatMessagePayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				continue
			}
			switch payload.EventType {
			case "text", "stream_delta":
				result.FullText += payload.Text
			case "result":
				// result.Text 是完整回复，不累加（stream_delta 已累加）
				result.CostUSD = payload.CostUSD
			case "tool_start":
				result.ToolCalls = append(result.ToolCalls, payload.ToolName)
			}

		case protocol.TypeChatReady:
			return result, nil

		case protocol.TypeChatError:
			var payload protocol.ErrorPayload
			json.Unmarshal(msg.Payload, &payload)
			result.IsError = true
			result.ErrorMsg = payload.Message
			return result, nil
		}
	}

	return result, scanner.Err()
}

// ClientsByClawbot 列出指定电脑上的客户端
func (b *Bridge) ClientsByClawbot(clawbotID string) ([]protocol.ClientInfo, error) {
	url := b.ServerURL + "/api/bot/clients?clawbot_id=" + clawbotID
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var clients []protocol.ClientInfo
	json.NewDecoder(resp.Body).Decode(&clients)
	return clients, nil
}

// AllClients 列出所有客户端
func (b *Bridge) AllClients() ([]protocol.ClientInfo, error) {
	url := b.ServerURL + "/api/bot/clients"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var clients []protocol.ClientInfo
	json.NewDecoder(resp.Body).Decode(&clients)
	return clients, nil
}
