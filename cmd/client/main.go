package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/shayin/claude-forward/internal/client"
)

// buildInfo 通过 ldflags 注入
var buildInfo string

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "", "配置文件路径（留空自动搜索 ~/.claude-forward/client.yaml 或 configs/client.yaml）")
	workDir := flag.String("dir", "", "工作目录，即 Claude 控制的项目目录（默认为当前目录）")
	showVersion := flag.Bool("version", false, "显示版本信息")
	flag.Parse()

	if *showVersion {
		if buildInfo != "" {
			fmt.Println(buildInfo)
		} else {
			fmt.Println("dev")
		}
		return
	}

	// 切换工作目录
	if *workDir != "" {
		if err := os.Chdir(*workDir); err != nil {
			log.Fatalf("Failed to change to work directory %s: %v", *workDir, err)
		}
	}

	// 查找并加载配置文件
	resolvedPath := client.ResolveConfigPath(*configPath)
	var config *client.Config
	if resolvedPath != "" {
		var err error
		config, err = client.LoadConfig(resolvedPath)
		if err != nil {
			log.Fatalf("Failed to load config %s: %v", resolvedPath, err)
		}
		log.Printf("Using config: %s", resolvedPath)
	} else {
		log.Printf("No config file found, using defaults")
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
