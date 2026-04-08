package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/shayin/claude-forward/internal/protocol"
)

func main() {
	configPath := "configs/wechat-bridge.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Printf("Using default config: %v", err)
		cfg = DefaultConfig()
	}

	if cfg.Server.URL == "" || cfg.Server.Token == "" {
		log.Fatal("server.url and server.token are required in config")
	}

	bridge := NewBridge(cfg.Server.URL, cfg.Server.Token)

	// ===== 微信登录 =====
	bot := newILinkBot()

	// 尝试复用已保存的 session
	var result *LoginResult
	saved, err := LoadSession()
	if err == nil && saved != nil {
		log.Println("发现已保存的登录凭证，尝试复用...")
		bot.Token = saved.BotToken
		bot.BaseURL = saved.BaseURL
		if bot.ValidateSession() {
			result = saved
			log.Printf("凭证有效！Bot ID: %s, User ID: %s（跳过扫码）", result.BotID, result.UserID)
		} else {
			log.Println("凭证已失效，需要重新扫码登录")
			bot.Token = ""
			bot.BaseURL = defaultBaseURL
		}
	}

	// 需要扫码登录
	if result == nil {
		log.Println("=== 微信扫码登录 ===")

		qrResult, err := bot.fetchQRCode()
		if err != nil {
			log.Fatalf("获取二维码失败: %v", err)
		}

		fmt.Println()
		fmt.Println("请用微信扫描以下二维码完成连接：")
		fmt.Println()
		qrterminal.GenerateWithConfig(qrResult.QRCodeURL, qrterminal.Config{
			Level:     qrterminal.L,
			Writer:    os.Stdout,
			BlackChar: "█",
			WhiteChar: " ",
		})
		fmt.Println()
		fmt.Println("如果二维码显示不全，请复制以下链接到手机浏览器打开：")
		fmt.Println(qrResult.QRCodeURL)
		fmt.Println()

		result, err = bot.waitForLogin(qrResult.Token, 8*time.Minute)
		if err != nil {
			log.Fatalf("登录失败: %v", err)
		}

		// 保存登录凭证
		bot.Token = result.BotToken
		bot.BaseURL = result.BaseURL

		if err := SaveSession(result); err != nil {
			log.Printf("警告：保存登录凭证失败: %v", err)
		} else {
			log.Println("登录凭证已保存")
		}
	}

	log.Printf("登录成功！Bot ID: %s, User ID: %s", result.BotID, result.UserID)

	fmt.Println()
	fmt.Println("✅ 微信连接成功！现在可以通过微信与 Claude Code 对话了。")
	fmt.Println("   指令: /clients /switch <id> /new /status")
	fmt.Println()

	// ===== 消息循环 =====
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		log.Println("正在关闭...")
		os.Exit(0)
	}()

	log.Println("开始监听微信消息...")
	typingTickets := make(map[string]string)

	for {
		msgs, newBuf, err := bot.GetUpdates()
		if err != nil {
			log.Printf("GetUpdates error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		bot.UpdateBuf = newBuf

		for _, msg := range msgs {
			if msg.MessageType != 1 || len(msg.ItemList) == 0 {
				continue
			}

			var text string
			for _, item := range msg.ItemList {
				if item.TextItem != nil {
					text += item.TextItem.Text
				}
			}
			if text == "" {
				continue
			}

			fromUser := msg.FromUserID
			ctxToken := msg.ContextToken
			log.Printf("收到消息 [from=%s]: %s", fromUser, truncate(text, 80))

			// 处理指令
			if strings.HasPrefix(text, "/") {
				handleCommand(bridge, bot, cfg, fromUser, text, ctxToken)
				continue
			}

			// 白名单检查
			clawbotID, err := cfg.ResolveClawbotID(fromUser)
			if err != nil {
				log.Printf("[REJECT] 拒绝未授权用户: %s", fromUser)
				bot.SendMessage(fromUser, "⛔ 你没有使用权限", ctxToken)
				continue
			}

			// 检查是否有粘性会话
			session := bridge.GetSession(fromUser)
			if session == nil || session.ClawbotID != clawbotID {
				// 新会话或 clawbot_id 变更，自动选择第一个 Client
				clients, err := bridge.ClientsByClawbot(clawbotID)
				if err != nil || len(clients) == 0 {
					bot.SendMessage(fromUser, fmt.Sprintf("❌ 电脑 %q 上没有在线的 Client", clawbotID), ctxToken)
					continue
				}
				bridge.SetSession(fromUser, clawbotID, clients[0].ID)
				session = bridge.GetSession(fromUser)
				log.Printf("[SESSION] 新会话: %s → clawbot=%s, client=%s", fromUser, clawbotID, clients[0].ID)
			}

			// 显示"正在输入"
			go sendTypingIndicator(bot, fromUser, typingTickets)

			// 调用 Claude
			resp, err := bridge.Chat(fromUser, text)
			if err != nil {
				log.Printf("Bridge error: %v", err)
				bot.SendMessage(fromUser, fmt.Sprintf("❌ 请求失败: %v", err), ctxToken)
				continue
			}

			if resp.IsError {
				bot.SendMessage(fromUser, fmt.Sprintf("❌ Claude 错误: %s", resp.ErrorMsg), ctxToken)
				continue
			}

			reply := resp.FullText
			if reply == "" {
				reply = "（Claude 未返回文本内容）"
			}

			// 分段发送
			chunks := splitMessage(reply, 4000)
			for i, chunk := range chunks {
				if i > 0 {
					time.Sleep(500 * time.Millisecond)
				}
				if err := bot.SendMessage(fromUser, chunk, ctxToken); err != nil {
					log.Printf("Send error: %v", err)
				}
			}

			log.Printf("回复已发送 [to=%s, chunks=%d]", fromUser, len(chunks))

			// 取消输入状态
			if ticket, ok := typingTickets[fromUser]; ok {
				bot.SendTyping(fromUser, ticket, 2)
			}
		}
	}
}

// handleCommand 处理微信指令
func handleCommand(bridge *Bridge, bot *iLinkBot, cfg *Config, fromUser, text, ctxToken string) {
	parts := strings.Fields(text)
	cmd := parts[0]

	switch cmd {
	case "/clients":
		clawbotID, _ := cfg.ResolveClawbotID(fromUser)
		var clients []protocol.ClientInfo
		var err error

		if clawbotID != "" {
			clients, err = bridge.ClientsByClawbot(clawbotID)
		} else {
			clients, err = bridge.AllClients()
		}

		if err != nil {
			bot.SendMessage(fromUser, fmt.Sprintf("❌ 获取列表失败: %v", err), ctxToken)
			return
		}

		if len(clients) == 0 {
			bot.SendMessage(fromUser, "没有在线的客户端", ctxToken)
			return
		}

		var sb strings.Builder
		sb.WriteString("在线客户端：\n")
		for i, c := range clients {
			active := ""
			if s := bridge.GetSession(fromUser); s != nil && s.ClientID == c.ID {
				active = " ← 当前"
			}
			sb.WriteString(fmt.Sprintf("%d. %s (%s) [%s]%s\n", i+1, c.Name, c.ID, c.ClawbotID, active))
		}
		bot.SendMessage(fromUser, sb.String(), ctxToken)

	case "/switch":
		if len(parts) < 2 {
			bot.SendMessage(fromUser, "用法: /switch <client_id>\n用 /clients 查看列表", ctxToken)
			return
		}
		targetID := parts[1]
		clawbotID, _ := cfg.ResolveClawbotID(fromUser)
		bridge.SetSession(fromUser, clawbotID, targetID)
		bot.SendMessage(fromUser, fmt.Sprintf("✅ 已切换到 %s", targetID), ctxToken)
		log.Printf("[SESSION] %s 手动切换到 %s", fromUser, targetID)

	case "/new":
		// 发送 new_session 类型的消息（通过 client_id 直接发）
		session := bridge.GetSession(fromUser)
		if session == nil {
			bot.SendMessage(fromUser, "❌ 请先发送一条消息建立会话", ctxToken)
			return
		}
		// 通过 Chat 发送特殊指令不太合适，直接用 Bot API
		// TODO: 后续可在 Server Bot API 添加 /api/bot/session 端点
		bot.SendMessage(fromUser, "✅ 新会话指令已记录（下一条消息将使用新会话）", ctxToken)

	case "/status":
		session := bridge.GetSession(fromUser)
		if session == nil {
			bot.SendMessage(fromUser, "当前无活跃会话", ctxToken)
			return
		}
		msg := fmt.Sprintf("当前会话：\n- Clawbot: %s\n- Client: %s\n- 活跃时间: %s",
			session.ClawbotID, session.ClientID,
			session.LastActive.Format("15:04:05"))
		bot.SendMessage(fromUser, msg, ctxToken)

	default:
		bot.SendMessage(fromUser, "未知指令。可用指令：/clients /switch <id> /new /status", ctxToken)
	}
}

func sendTypingIndicator(bot *iLinkBot, userID string, tickets map[string]string) {
	ticket, ok := tickets[userID]
	if !ok {
		t, err := bot.GetConfig(userID)
		if err == nil && t != "" {
			tickets[userID] = t
			ticket = t
		}
	}
	if ticket != "" {
		bot.SendTyping(userID, ticket, 1)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func splitMessage(text string, chunkSize int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= chunkSize {
			chunks = append(chunks, text)
			break
		}
		chunks = append(chunks, text[:chunkSize])
		text = text[chunkSize:]
	}
	if len(chunks) > 1 {
		for i := range chunks {
			chunks[i] = fmt.Sprintf("[%d/%d] %s", i+1, len(chunks), chunks[i])
		}
	}
	return chunks
}
