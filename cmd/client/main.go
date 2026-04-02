package main

import (
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/shayin/claude-forward/internal/client"
)

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "configs/client.yaml", "配置文件路径")
	flag.Parse()

	// 加载配置
	config, err := client.LoadConfig(*configPath)
	if err != nil {
		log.Printf("Using default config: %v", err)
		config = client.DefaultConfig()
		client.ApplyDefaults(config)
	}

	// 重复检测：检查 tmux session 是否已存在
	if tmuxAvailable(config.Tmux.SessionName) {
		log.Fatalf("tmux session '%s' already exists. Is another client running for this project? Run `tmux kill-session -t %s` to clean up.", config.Tmux.SessionName, config.Tmux.SessionName)
	}

	log.Printf("Starting client: id=%s name=%s tmux_session=%s", config.Client.ID, config.Client.Name, config.Tmux.SessionName)

	// 创建客户端
	c := client.NewClient(config)

	// 信号处理
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 启动客户端
	go c.Run()

	// 等待信号
	<-sigChan
	log.Println("Shutting down client...")
	c.Disconnect()

	// 清理 tmux session
	killTmuxSession(config.Tmux.SessionName)
}

// tmuxAvailable 检查 tmux 是否可用且指定 session 已存在
func tmuxAvailable(sessionName string) bool {
	if _, err := exec.LookPath("tmux"); err != nil {
		return false
	}
	cmd := exec.Command("tmux", "has-session", "-t", sessionName)
	return cmd.Run() == nil
}

// killTmuxSession 清理 tmux session
func killTmuxSession(sessionName string) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return
	}
	cmd := exec.Command("tmux", "kill-session", "-t", sessionName)
	if err := cmd.Run(); err == nil {
		log.Printf("Killed tmux session '%s'", sessionName)
	}
}
