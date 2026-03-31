package client

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// TmuxManager tmux 会话管理器
type TmuxManager struct {
	config   TmuxConfig
	pty      *os.File
	ptyMu    sync.Mutex
	cmd      *exec.Cmd
	lastSize struct {
		cols int
		rows int
	}
	useTmux bool // 是否使用 tmux
}

// NewTmuxManager 创建 tmux 管理器
func NewTmuxManager(config TmuxConfig) *TmuxManager {
	// 检查 tmux 是否可用
	_, err := exec.LookPath("tmux")
	return &TmuxManager{
		config:  config,
		useTmux: err == nil,
	}
}

// SessionExists 检查会话是否存在
func (t *TmuxManager) SessionExists() bool {
	if !t.useTmux {
		return t.pty != nil
	}
	cmd := exec.Command("tmux", "has-session", "-t", t.config.SessionName)
	return cmd.Run() == nil
}

// CreateSession 创建会话
func (t *TmuxManager) CreateSession() error {
	shell := t.config.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
	}

	if t.useTmux {
		cmd := exec.Command("tmux", "new-session", "-d", "-s", t.config.SessionName, shell)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("tmux new-session failed: %s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	}
	return nil
}

// EnsureSession 确保会话存在，失败时重试
func (t *TmuxManager) EnsureSession() error {
	if t.SessionExists() {
		return nil
	}
	// 第一次尝试创建
	if err := t.CreateSession(); err == nil {
		return nil
	}
	// 失败后清理残留状态再重试
	_ = exec.Command("tmux", "kill-server").Run()
	time.Sleep(500 * time.Millisecond)
	return t.CreateSession()
}

// KillSession 终止会话
func (t *TmuxManager) KillSession() error {
	t.Close()
	if t.useTmux {
		cmd := exec.Command("tmux", "kill-session", "-t", t.config.SessionName)
		return cmd.Run()
	}
	return nil
}

// Attach 通过 PTY 连接到会话
func (t *TmuxManager) Attach() error {
	t.ptyMu.Lock()
	defer t.ptyMu.Unlock()

	// 检查 PTY 是否仍然有效（进程是否还在运行）
	if t.pty != nil {
		if t.cmd != nil && t.cmd.Process != nil {
			// 检查进程是否已经退出
			if err := t.cmd.Process.Signal(syscall.Signal(0)); err != nil {
				// 进程已退出，清理旧连接
				t.pty.Close()
				t.pty = nil
				t.cmd = nil
			} else {
				// 进程仍在运行，直接返回
				return nil
			}
		} else {
			// 没有 cmd 或进程，清理旧连接
			t.pty.Close()
			t.pty = nil
		}
	}

	shell := t.config.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
	}

	if t.useTmux {
		// 使用 tmux attach 连接到会话
		t.cmd = exec.Command("tmux", "attach", "-t", t.config.SessionName)
	} else {
		// 直接启动 shell
		t.cmd = exec.Command(shell)
	}

	// 启动 PTY
	ptmx, err := pty.Start(t.cmd)
	if err != nil {
		return fmt.Errorf("failed to start pty: %w", err)
	}

	t.pty = ptmx
	return nil
}

// Close 关闭 PTY 连接
func (t *TmuxManager) Close() error {
	t.ptyMu.Lock()
	defer t.ptyMu.Unlock()

	if t.pty != nil {
		t.pty.Close()
		t.pty = nil
	}
	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Signal(syscall.SIGTERM)
		t.cmd = nil
	}
	return nil
}

// Write 写入数据
func (t *TmuxManager) Write(data string) error {
	t.ptyMu.Lock()
	defer t.ptyMu.Unlock()

	if t.pty == nil {
		return fmt.Errorf("pty not attached")
	}

	_, err := t.pty.Write([]byte(data))
	return err
}

// Read 读取输出
func (t *TmuxManager) Read(buf []byte) (int, error) {
	t.ptyMu.Lock()
	ptyFile := t.pty
	t.ptyMu.Unlock()

	if ptyFile == nil {
		return 0, fmt.Errorf("pty not attached")
	}

	// 设置读取超时
	ptyFile.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, err := ptyFile.Read(buf)
	if err != nil {
		if os.IsTimeout(err) {
			return 0, nil // 超时返回 0，不报错
		}
		return n, err
	}
	return n, nil
}

// Resize 调整终端大小
func (t *TmuxManager) Resize(cols, rows int) error {
	t.ptyMu.Lock()
	defer t.ptyMu.Unlock()

	if t.pty == nil {
		return nil
	}

	if t.lastSize.cols == cols && t.lastSize.rows == rows {
		return nil
	}

	t.lastSize.cols = cols
	t.lastSize.rows = rows

	return pty.Setsize(t.pty, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}

// SendKeys 直接发送按键（tmux 模式）
func (t *TmuxManager) SendKeys(keys string) error {
	if !t.useTmux {
		return fmt.Errorf("tmux not available")
	}
	cmd := exec.Command("tmux", "send-keys", "-t", t.config.SessionName, keys)
	return cmd.Run()
}

// CaptureOutput 捕获当前屏幕输出（包含 ANSI 转义序列）
func (t *TmuxManager) CaptureOutput() (string, error) {
	if !t.useTmux {
		return "", fmt.Errorf("tmux not available")
	}
	// 使用 -e 参数保留 ANSI 转义序列，这样 xterm.js 可以正确渲染
	cmd := exec.Command("tmux", "capture-pane", "-t", t.config.SessionName, "-p", "-e")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// RunInSession 在会话中运行命令
func (t *TmuxManager) RunInSession(cmdStr string) error {
	if t.useTmux {
		return exec.Command("tmux", "send-keys", "-t", t.config.SessionName, cmdStr, "Enter").Run()
	}
	// 直接模式：写入命令
	return t.Write(cmdStr + "\n")
}

// StartClaude 在会话中启动 Claude Code
func (t *TmuxManager) StartClaude() error {
	return t.RunInSession("claude")
}

// ListSessions 列出所有会话
func (t *TmuxManager) ListSessions() ([]string, error) {
	if !t.useTmux {
		return nil, fmt.Errorf("tmux not available")
	}
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}

	sessions := strings.Split(strings.TrimSpace(out.String()), "\n")
	return sessions, nil
}

// IsAttached 检查是否已连接
func (t *TmuxManager) IsAttached() bool {
	t.ptyMu.Lock()
	defer t.ptyMu.Unlock()
	return t.pty != nil
}

// EnsureReader 确保有一个可用的 Reader
func (t *TmuxManager) EnsureReader() (io.Reader, error) {
	if err := t.Attach(); err != nil {
		return nil, err
	}
	return t.pty, nil
}
