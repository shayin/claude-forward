.PHONY: all build build-server build-client build-cli build-wechat-bridge install-client clean test run-server run-client run-wechat-bridge

BINARY_SERVER=bin/server
BINARY_CLIENT=bin/client
BINARY_CLI=bin/cli
BINARY_WECHAT_BRIDGE=bin/wechat-bridge

all: build

build: build-server build-client

BUILD_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date '+%Y-%m-%d_%H:%M:%S')
LDFLAGS := -X 'main.buildInfo=$(BUILD_VERSION) $(BUILD_TIME)'

build-server:
	@echo "Building server..."
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_SERVER) ./cmd/server

build-client:
	@echo "Building client..."
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_CLIENT) ./cmd/client

build-cli:
	@echo "Building CLI..."
	@mkdir -p bin
	go build -o $(BINARY_CLI) ./cmd/cli

build-wechat-bridge:
	@echo "Building wechat-bridge..."
	@mkdir -p bin
	go build -o $(BINARY_WECHAT_BRIDGE) ./cmd/wechat-bridge

install-client: build-client
	@mkdir -p $(HOME)/.claude-forward
	cp bin/client $(HOME)/.claude-forward/client
	@if [ ! -L /usr/local/bin/cf ]; then \
		ln -sf $(HOME)/.claude-forward/client /usr/local/bin/cf 2>/dev/null || \
		echo "提示: 无法创建 /usr/local/bin/cf，请手动执行: sudo ln -sf $$HOME/.claude-forward/client /usr/local/bin/cf"; \
	fi
	@if [ ! -f $(HOME)/.claude-forward/client.yaml ]; then \
		cp configs/client.yaml.tpl $(HOME)/.claude-forward/client.yaml; \
		echo "已创建配置文件: ~/.claude-forward/client.yaml，请编辑填写服务器信息"; \
	fi
	@echo "安装完成！使用: cf"

clean:
	@echo "Cleaning..."
	@rm -rf bin/

test:
	go test -v ./...

run-server:
	go run ./cmd/server

run-client:
	go run ./cmd/client

run-wechat-bridge:
	go run ./cmd/wechat-bridge

# 前端构建
build-web:
	@echo "Building web UI..."
	cd web && npm install && npm run build

# 生成自签名证书
gen-cert:
	@echo "Generating self-signed certificate..."
	@mkdir -p certs
	openssl req -x509 -newkey rsa:4096 -keyout certs/key.pem -out certs/cert.pem -days 365 -nodes -subj "/CN=localhost"

# 安装依赖
deps:
	go mod download
	go mod tidy
