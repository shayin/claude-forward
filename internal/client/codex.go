package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// openEngineStdoutLog 打开引擎 stdout 留存日志（诊断用）。
// 路径 ~/.claude-forward/logs/<engine>-<clientID>-YYYY-MM-DD.jsonl，按 clientID + 日期分文件。
// 失败返回 nil（不影响主流程，只是没有留存）。
func openEngineStdoutLog(engine, clientID string) *os.File {
	dir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	logDir := filepath.Join(dir, ".claude-forward", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil
	}
	date := time.Now().Format("2006-01-02")
	name := fmt.Sprintf("%s-%s-%s.jsonl", engine, clientID, date)
	if clientID == "" {
		name = fmt.Sprintf("%s-%s.jsonl", engine, date)
	}
	f, err := os.OpenFile(filepath.Join(logDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}
	return f
}

// CodexManager 管理 Codex CLI（codex exec）调用，实现 Runner interface。
// 与 ClaudeManager 镜像相同的事件模型（ClaudeEvent），供 handleChatInput 在
// bot 通道按 botEngine 选择。codex 输出 JSONL（每行一个 ThreadEvent），
// 由 codexParser 解析为 ClaudeEvent。
type CodexManager struct {
	config       CodexConfig
	sessionID    string // Web UI 会话 ID（codex 实际不用于 Web UI，保留接口对称）
	botSessionID string // Bot API（微信端）的 codex thread_id，独立持久化
	model        string // 运行时可改的模型（初始从 config.Model 读），通过 /model 命令切换
	cancel       context.CancelFunc
	events       chan ClaudeEvent
	mu           sync.Mutex
	running      bool
}

// 编译期断言：CodexManager 满足 Runner 接口
var _ Runner = (*CodexManager)(nil)

// NewCodexManager 创建 Codex 管理器
func NewCodexManager(config CodexConfig) *CodexManager {
	if config.Path == "" {
		config.Path = "codex"
	}
	if config.Sandbox == "" {
		config.Sandbox = "workspace-write"
	}
	cm := &CodexManager{
		config:  config,
		model:   config.Model, // 初始从配置读，运行时可通过 /model 命令切换
		running: false,
	}
	cm.loadSessionID()
	cm.loadBotSessionID()
	return cm
}

// SessionID 返回 Web UI 会话 ID（codex 不实际使用）
func (cm *CodexManager) SessionID() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.sessionID
}

// SetSessionID 设置 Web UI 会话 ID
func (cm *CodexManager) SetSessionID(id string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.sessionID = id
	cm.saveSessionID()
}

// BotSessionID 返回 Bot API（微信端）的 thread_id
func (cm *CodexManager) BotSessionID() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.botSessionID
}

// SetBotSessionID 设置 Bot API 的 thread_id，并持久化
func (cm *CodexManager) SetBotSessionID(id string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.botSessionID = id
	cm.saveBotSessionID()
}

// IsRunning 是否正在运行
func (cm *CodexManager) IsRunning() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.running
}

// GetModel 返回当前 codex 模型（运行时可被 /model 命令修改）
func (cm *CodexManager) GetModel() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.model
}

// SetModel 运行时切换 codex 模型（由 /model 命令调用）
func (cm *CodexManager) SetModel(m string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.model = m
}

// SendMessage 发送消息给 Codex，启动一个 `codex exec` 子进程。
// resumeSessionID 非空时用 `codex exec resume <id>` 续接该 thread，为空则全新会话。
func (cm *CodexManager) SendMessage(text string, resumeSessionID string) error {
	ctx, cancel := context.WithCancel(context.Background())
	cm.mu.Lock()
	if cm.running {
		cm.mu.Unlock()
		cancel()
		return fmt.Errorf("codex is still processing")
	}
	// cancel 在锁内赋值，避免与 Abort() 竞态导致孤儿进程
	cm.cancel = cancel
	cm.events = make(chan ClaudeEvent, 256)
	cm.running = true
	cm.mu.Unlock()

	// 构建命令参数（全局 flag 必须在 resume 子命令之前——codex 0.144.x 实测要求）
	// 全新会话：codex exec --json --sandbox <mode> "<text>"
	// resume：   codex exec --json resume <thread_id> "<text>"（sandbox 由原 thread 继承）
	args := []string{"exec", "--json", "--color", "never", "--skip-git-repo-check"}
	if resumeSessionID == "" {
		// 全新会话才指定 sandbox；resume 继承原 thread 的设置
		if cm.config.Sandbox == "dangerously-bypass-approvals-and-sandbox" {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, "--sandbox", cm.config.Sandbox)
		}
	}
	if model := cm.GetModel(); model != "" {
		args = append(args, "-m", model)
	}
	if resumeSessionID != "" {
		args = append(args, "resume", resumeSessionID)
	}
	args = append(args, text)

	cmd := exec.CommandContext(ctx, cm.config.Path, args...)
	cmd.Dir = cm.getWorkDir()
	// codex 用自己的登录态（~/.codex/），继承当前环境即可
	cmd.Env = os.Environ()

	// stderr 重定向到文件（按 clientID 区分）
	logName := "claude-forward-codex.log"
	if cm.config.ClientID != "" {
		logName = fmt.Sprintf("claude-forward-codex-%s.log", cm.config.ClientID)
	}
	logFile, err := os.OpenFile(filepath.Join(os.TempDir(), logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stderr = logFile
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cm.setRunning(false)
		cancel()
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cm.setRunning(false)
		cancel()
		return fmt.Errorf("failed to start codex: %w", err)
	}

	log.Printf("Codex process started: pid=%d args=%v", cmd.Process.Pid, args)

	// stdout 留存日志（诊断用）：~/.claude-forward/logs/codex-<clientID>-YYYY-MM-DD.jsonl
	stdoutLog := openEngineStdoutLog("codex", cm.config.ClientID)

	// resume 时记下期望的 thread_id，用于 thread.started 校验（codex 对非法 id 会静默新建 thread）
	expectedThread := resumeSessionID

	go func() {
		defer func() {
			close(cm.events)
			cm.setRunning(false)
			cancel()
			if logFile != nil {
				logFile.Close()
			}
			if stdoutLog != nil {
				stdoutLog.Close()
			}
		}()

		parser := &codexParser{expectedThread: expectedThread}

		// stdout 经 TeeReader 同时写入留存日志（便于事后排查"未返回文本"等问题）
		reader := io.Reader(stdout)
		if stdoutLog != nil {
			reader = io.TeeReader(stdout, stdoutLog)
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		sentAny := false // 跟踪是否发出过事件，用于进程异常退出时补错误反馈
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			events := parser.parse(line)
			for _, event := range events {
				sentAny = true
				select {
				case cm.events <- event:
				case <-ctx.Done():
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			log.Printf("Codex stdout scanner error: %v", err)
			cm.events <- ClaudeEvent{Type: EventError, Text: fmt.Sprintf("codex stream error: %v", err)}
			sentAny = true
		}

		if err := cmd.Wait(); err != nil {
			log.Printf("Codex process exited with error: %v", err)
			// 进程启动后立即失败且全程无输出（如 flag 错误、二进制不存在），
			// 补发错误事件，否则用户收到 ack 后无限沉默
			if !sentAny {
				cm.events <- ClaudeEvent{Type: EventError, Text: fmt.Sprintf("codex 启动失败，未产生输出: %v", err)}
			}
		}
	}()

	return nil
}

// Stream 返回事件 channel。
// 约束：必须在 SendMessage 返回成功后调用，且调用方保证无并发 SendMessage
//（cm.events 在 SendMessage 内创建、goroutine 内 close，依赖 happens-before 时序）。
func (cm *CodexManager) Stream() <-chan ClaudeEvent {
	return cm.events
}

// Abort 终止当前 Codex 进程
func (cm *CodexManager) Abort() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.cancel != nil {
		cm.cancel()
		cm.cancel = nil
	}
	cm.running = false
}

// ResetSession 重置 Web UI 会话（开始新会话）
func (cm *CodexManager) ResetSession() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.sessionID = ""
	cm.saveSessionID()
}

func (cm *CodexManager) setRunning(r bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.running = r
}

func (cm *CodexManager) getWorkDir() string {
	if cm.config.WorkDir != "" {
		return cm.config.WorkDir
	}
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

// sessionIDPath 返回 codex Web UI session_id 持久化文件路径（按 clientID 区分）
// 文件名带 _codex 后缀，与 cc 的 session_id 隔离
func (cm *CodexManager) sessionIDPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	name := "session_id_codex"
	if cm.config.ClientID != "" {
		name = fmt.Sprintf("session_id_codex_%s", cm.config.ClientID)
	}
	return filepath.Join(dir, ".claude-forward", name)
}

func (cm *CodexManager) saveSessionID() {
	path := cm.sessionIDPath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	if cm.sessionID == "" {
		os.Remove(path)
	} else {
		os.WriteFile(path, []byte(cm.sessionID), 0644)
	}
}

func (cm *CodexManager) loadSessionID() {
	path := cm.sessionIDPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	id := strings.TrimSpace(string(data))
	if id != "" {
		cm.sessionID = id
		log.Printf("Restored codex session_id from file: %s", id)
	}
}

// botSessionIDPath 返回 codex Bot API thread_id 持久化文件路径（按 clientID 区分）
func (cm *CodexManager) botSessionIDPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	name := "session_id_bot_codex"
	if cm.config.ClientID != "" {
		name = fmt.Sprintf("session_id_bot_codex_%s", cm.config.ClientID)
	}
	return filepath.Join(dir, ".claude-forward", name)
}

func (cm *CodexManager) saveBotSessionID() {
	path := cm.botSessionIDPath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	if cm.botSessionID == "" {
		os.Remove(path)
	} else {
		os.WriteFile(path, []byte(cm.botSessionID), 0644)
	}
}

func (cm *CodexManager) loadBotSessionID() {
	path := cm.botSessionIDPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	id := strings.TrimSpace(string(data))
	if id != "" {
		cm.botSessionID = id
		log.Printf("Restored codex bot session_id from file: %s", id)
	}
}

// ---- codex JSONL 解析 ----

// codexParser 解析 codex exec --json 的 JSONL 流，转换为 ClaudeEvent。
// 状态：expectedThread 用于 resume 校验（codex 对非法 thread_id 会静默新建 thread）。
type codexParser struct {
	expectedThread string
}

// codexUsage 对应 turn.completed 的 usage 字段
type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// codexThreadError 对应 turn.failed 的 error 字段
type codexThreadError struct {
	Message string `json:"message"`
}

// codexItem 对应 item.started / item.completed 的 item 对象（ThreadItem）
// 各 type 用到的字段不同，未用到的保持零值
type codexItem struct {
	ID   string `json:"id"`
	Type string `json:"type"` // agent_message | reasoning | command_execution | file_change | mcp_tool_call | web_search | todo_list | error

	Text string `json:"text"` // agent_message, reasoning

	Command          string `json:"command"`           // command_execution
	AggregatedOutput string `json:"aggregated_output"` // command_execution
	ExitCode         *int   `json:"exit_code"`         // command_execution
	Status           string `json:"status"`            // command_execution, file_change, mcp_tool_call

	Changes []codexFileChange `json:"changes"` // file_change

	Server    string          `json:"server"`    // mcp_tool_call
	Tool      string          `json:"tool"`      // mcp_tool_call
	Arguments json.RawMessage `json:"arguments"` // mcp_tool_call
	Result    json.RawMessage `json:"result"`    // mcp_tool_call

	Query   string `json:"query"` // web_search（字段名待 codex 实测确认）
	Message string `json:"message"` // error
}

type codexFileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // add | delete | update
}

// parse 解析单行 JSONL，可能产出 0 或多个 ClaudeEvent
func (p *codexParser) parse(line string) []ClaudeEvent {
	var raw struct {
		Type     string            `json:"type"`
		ThreadID string            `json:"thread_id"` // thread.started
		Item     json.RawMessage   `json:"item"`      // item.*
		Usage    *codexUsage       `json:"usage"`     // turn.completed
		Message  string            `json:"message"`   // error
		Error    *codexThreadError `json:"error"`     // turn.failed
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "thread.started":
		if raw.ThreadID == "" {
			return nil
		}
		if p.expectedThread != "" && p.expectedThread != raw.ThreadID {
			log.Printf("codex resume returned different thread_id, expected=%s got=%s (likely invalid resume ID, started new thread)", p.expectedThread, raw.ThreadID)
		}
		return []ClaudeEvent{{Type: EventInit, SessionID: raw.ThreadID}}

	case "item.started":
		return parseCodexItemStarted(raw.Item)

	case "item.completed":
		return parseCodexItemCompleted(raw.Item)

	case "turn.completed":
		// usage 可能为 nil（异常场景），仍发空 EventResult 让 handleChatInput 收到轮次结束信号
		evt := ClaudeEvent{Type: EventResult}
		if raw.Usage != nil {
			// codex 订阅模式无 cost，CacheRead 用 cached_input_tokens 近似
			evt.InputTokens = raw.Usage.InputTokens
			evt.OutputTokens = raw.Usage.OutputTokens + raw.Usage.ReasoningOutputTokens
			evt.CacheReadInputTokens = raw.Usage.CachedInputTokens
		}
		return []ClaudeEvent{evt}

	case "turn.failed":
		msg := "codex turn failed"
		if raw.Error != nil && raw.Error.Message != "" {
			msg = raw.Error.Message
		}
		return []ClaudeEvent{{Type: EventError, Text: msg}}

	case "error":
		// 顶层致命错误
		msg := raw.Message
		if msg == "" {
			msg = "codex error"
		}
		return []ClaudeEvent{{Type: EventError, Text: msg}}
	}

	// 已知但 v1 忽略：turn.started / item.updated；未知 type 打 log 便于排查 codex 未来新增事件
	switch raw.Type {
	case "turn.started", "item.updated":
	default:
		log.Printf("codexParser: unhandled event type %q: %s", raw.Type, line)
	}
	return nil
}

// parseCodexItemStarted 处理 item.started，对有执行语义的工具发 EventToolStart
// agent_message 和 reasoning codex 不 emit item.started，无需处理
func parseCodexItemStarted(itemRaw json.RawMessage) []ClaudeEvent {
	if len(itemRaw) == 0 {
		return nil
	}
	var item codexItem
	if err := json.Unmarshal(itemRaw, &item); err != nil {
		return nil
	}
	switch item.Type {
	case "command_execution":
		input, _ := json.Marshal(map[string]string{"command": item.Command})
		return []ClaudeEvent{{Type: EventToolStart, ToolID: item.ID, ToolName: "shell", ToolInput: input}}
	case "file_change":
		return []ClaudeEvent{{Type: EventToolStart, ToolID: item.ID, ToolName: "apply_patch", ToolInput: itemRaw}}
	case "web_search":
		input := []byte("null")
		if item.Query != "" {
			input, _ = json.Marshal(map[string]string{"query": item.Query})
		}
		return []ClaudeEvent{{Type: EventToolStart, ToolID: item.ID, ToolName: "web_search", ToolInput: input}}
	case "mcp_tool_call":
		args := json.RawMessage(item.Arguments)
		if len(args) == 0 {
			args = json.RawMessage("null") // 避免 Marshal 空 RawMessage 报错被静默丢弃
		}
		input, _ := json.Marshal(map[string]any{"server": item.Server, "tool": item.Tool, "arguments": args})
		return []ClaudeEvent{{Type: EventToolStart, ToolID: item.ID, ToolName: item.Tool, ToolInput: input}}
	}
	return nil
}

// parseCodexItemCompleted 处理 item.completed，按 item.type 分发到对应事件
func parseCodexItemCompleted(itemRaw json.RawMessage) []ClaudeEvent {
	if len(itemRaw) == 0 {
		return nil
	}
	var item codexItem
	if err := json.Unmarshal(itemRaw, &item); err != nil {
		return nil
	}
	switch item.Type {
	case "agent_message":
		if item.Text == "" {
			return nil
		}
		return []ClaudeEvent{{Type: EventText, Text: item.Text}}
	case "reasoning":
		if item.Text == "" {
			return nil
		}
		return []ClaudeEvent{{Type: EventThinking, Text: item.Text}}
	case "command_execution":
		out := item.AggregatedOutput
		if item.Status != "" && item.Status != "completed" {
			out = fmt.Sprintf("[%s] %s", item.Status, out)
		}
		return []ClaudeEvent{{Type: EventToolEnd, ToolID: item.ID, ToolName: "shell", ToolOutput: out}}
	case "file_change":
		out, _ := json.Marshal(item.Changes)
		return []ClaudeEvent{{Type: EventToolEnd, ToolID: item.ID, ToolName: "apply_patch", ToolOutput: string(out)}}
	case "mcp_tool_call":
		return []ClaudeEvent{{Type: EventToolEnd, ToolID: item.ID, ToolName: item.Tool, ToolOutput: string(item.Result)}}
	case "web_search":
		return []ClaudeEvent{{Type: EventToolEnd, ToolID: item.ID, ToolName: "web_search", ToolOutput: item.Query}}
	case "error":
		// item 内非致命 error（config warning 等），仍传递给用户
		return []ClaudeEvent{{Type: EventError, Text: item.Message}}
	}
	return nil
}
