package client

// Runner 抽象 AI 引擎（Claude Code / Codex CLI）的会话与生命周期操作。
// handleChatInput 在 bot 通道按当前引擎（botEngine）选择具体实现，
// Web UI 通道始终使用 ClaudeManager。
//
// 两种引擎复用同一个 ClaudeEvent struct 和事件类型常量，
// 因此 handleChatInput 的事件循环、后台模式收集、错误推送等逻辑可以零改动复用。
type Runner interface {
	// IsRunning 当前是否有任务在执行（用于拒绝并发请求）
	IsRunning() bool
	// SendMessage 启动一次引擎调用。resumeSessionID 非空时续接该会话，为空则全新会话
	SendMessage(text string, resumeSessionID string) error
	// Stream 返回事件 channel，引擎自行在结束时 close
	Stream() <-chan ClaudeEvent
	// Abort 终止当前引擎进程
	Abort()
	// SessionID / SetSessionID Web UI 通道的会话 ID（codex 实际不用，保留接口对称）
	SessionID() string
	SetSessionID(id string)
	// BotSessionID / SetBotSessionID 微信 Bot 通道的会话 ID，独立持久化
	BotSessionID() string
	SetBotSessionID(id string)
	// ResetSession 清空 Web UI 会话 ID 并持久化（保持现有语义：不清 bot session）
	ResetSession()
}

// 编译期断言：ClaudeManager 满足 Runner 接口
var _ Runner = (*ClaudeManager)(nil)
