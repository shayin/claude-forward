package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/shayin/claude-forward/internal/client"
)

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "configs/client.yaml", "配置文件路径")
	flag.Parse()

	// 如果配置路径是相对路径，转换为相对于可执行文件的绝对路径
	if !filepath.IsAbs(*configPath) {
		execPath, err := os.Executable()
		if err == nil {
			execDir := filepath.Dir(execPath)
			*configPath = filepath.Join(execDir, "..", *configPath)
		}
	}

	// 加载配置
	config, err := client.LoadConfig(*configPath)
	if err != nil {
		log.Printf("Using default config: %v", err)
		config = client.DefaultConfig()
	}

	// 生成客户端 ID（如果未配置）
	if config.Client.ID == "" {
		hostname, _ := os.Hostname()
		config.Client.ID = hostname
		if config.Client.Name == "" {
			config.Client.Name = hostname
		}
	}

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
}
