.PHONY: all build build-server build-client build-cli clean test run-server run-client

BINARY_SERVER=bin/server
BINARY_CLIENT=bin/client
BINARY_CLI=bin/cli

all: build

build: build-server build-client

build-server: inject-build-info
	@echo "Building server..."
	@mkdir -p bin
	go build -o $(BINARY_SERVER) ./cmd/server

build-client:
	@echo "Building client..."
	@mkdir -p bin
	go build -o $(BINARY_CLIENT) ./cmd/client

inject-build-info:
	@GIT_VER=$$(git describe --tags --always --dirty 2>/dev/null || echo "unknown") && \
	BUILD_TIME=$$(date '+%Y-%m-%d %H:%M:%S') && \
	sed -i.bak "s|__BUILD_INFO__|$${GIT_VER} · $${BUILD_TIME}|" web/index.html && \
	rm -f web/index.html.bak

build-cli:
	@echo "Building CLI..."
	@mkdir -p bin
	go build -o $(BINARY_CLI) ./cmd/cli

clean:
	@echo "Cleaning..."
	@rm -rf bin/

test:
	go test -v ./...

run-server:
	go run ./cmd/server

run-client:
	go run ./cmd/client

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
