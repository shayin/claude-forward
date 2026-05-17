package main

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
	"github.com/shayin/claude-forward/internal/server"
)

// buildInfo 通过 ldflags 注入
var buildInfo string

func main() {
	// 加载配置
	configPath := "configs/server.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	config, err := server.LoadConfig(configPath)
	if err != nil {
		log.Printf("Using default config: %v", err)
		config = server.DefaultConfig()
	}

	// 初始化组件
	hub := server.NewHub()
	auth := server.NewAuth(config.Auth.Tokens)

	// 加密密钥
	var encryptionKey []byte
	if config.Server.EncryptionKey != "" {
		encryptionKey = protocol.DeriveKey(config.Server.EncryptionKey)
		log.Printf("Application-layer encryption enabled")
	}
	handler := server.NewHandler(hub, auth, encryptionKey)

	// 启动 Hub
	go hub.Run()

	// 设置路由
	http.HandleFunc("/ws", handler.HandleWS)
	http.HandleFunc("/api/clients", func(w http.ResponseWriter, r *http.Request) {
		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		if r.Method == "OPTIONS" {
			return
		}

		clients := hub.ListClients()
		w.Write(mustMarshal(clients))
	})

	// Bot API（供微信桥接等第三方服务调用）
	botHandler := server.NewBotHandler(hub, auth)
	http.HandleFunc("/api/bot/chat", botHandler.HandleChat)
	http.HandleFunc("/api/bot/clients", botHandler.HandleClients)

	// 微信集成（内置 iLink 支持）
	if config.WeChat.Enabled {
		wechatMgr := server.NewWeChatManager(hub, auth, config.WeChat)
		wechatHandler := server.NewWeChatHandler(wechatMgr, auth)
		http.HandleFunc("/api/wechat/status", wechatHandler.HandleStatus)
		http.HandleFunc("/api/wechat/qrcode/", wechatHandler.HandleQRCode)
		http.HandleFunc("/api/wechat/relogin/", wechatHandler.HandleRelogin)
		go wechatMgr.Start()
		defer wechatMgr.Stop()
		log.Println("WeChat integration enabled")
	}

	// 构建信息 API
	http.HandleFunc("/api/build-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(buildInfo))
	})

	// 静态文件服务（带 gzip 压缩）
	fs := http.FileServer(http.Dir("web"))
	http.Handle("/", gzipMiddleware(fs))

	// 创建 HTTP 服务器
	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: nil,
	}

	// 启动服务器
	go func() {
		log.Printf("Server starting on %s", addr)

		if config.Server.TLS.Enabled {
			var cert tls.Certificate
			var err error

			// 如果配置了域名，使用 Let's Encrypt
			// if config.Server.TLS.Domain != "" {
			// 	// TODO: 使用 certmagic 自动获取证书
			// }

			// 否则使用自签名证书或指定证书
			if config.Server.TLS.CertFile != "" && config.Server.TLS.KeyFile != "" {
				cert, err = tls.LoadX509KeyPair(config.Server.TLS.CertFile, config.Server.TLS.KeyFile)
			} else {
				// 生成自签名证书
				cert, err = generateSelfSignedCert()
			}

			if err != nil {
				log.Fatalf("Failed to load/generate certificate: %v", err)
			}

			srv.TLSConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
			}

			log.Printf("TLS enabled (self-signed certificate)")
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	srv.Close()
}

// generateSelfSignedCert 生成自签名证书
func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Claude Forward"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	return tls.X509KeyPair(certPEM, keyPEM)
}

func mustMarshal(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// gzipMiddleware 对文本类静态资源做 gzip 压缩
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 检查客户端是否支持 gzip
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// 只压缩文本类资源
		ext := ""
		if idx := strings.LastIndex(r.URL.Path, "."); idx >= 0 {
			ext = r.URL.Path[idx:]
		}
		switch ext {
		case ".js", ".css", ".html", ".json", ".svg", ".md":
			// 继续压缩
		default:
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()

		next.ServeHTTP(gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

// gzipResponseWriter 包装 ResponseWriter 以写入 gzip
type gzipResponseWriter struct {
	http.ResponseWriter
	io.Writer
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}
