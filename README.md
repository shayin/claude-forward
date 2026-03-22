# Claude Forward

自托管的远程编程系统，让你可以通过手机或其他设备控制家中内网电脑上的 Claude Code。

## 特性

- 🚀 **零成本部署** - 使用自签名证书，无需域名
- 📱 **双模式访问** - Web UI + CLI/Terminal
- 🔄 **断线重连** - 集成 tmux，网络中断不会丢失会话
- 🔐 **安全** - Token 认证 + TLS 加密
- 📊 **多会话** - 支持多个客户端同时连接

## 架构

```
┌─────────────┐                    ┌─────────────────┐                    ┌──────────────────┐
│   手机端    │                    │  云服务器 (CVM)  │                    │   本地电脑(内网)  │
│             │                    │                 │                    │                  │
│ ┌─────────┐ │    wss://...       │ ┌─────────────┐ │    wss://...       │ ┌──────────────┐ │
│ │ Web UI  │ │◄──────────────────►│ │   Server    │ │◄──────────────────►│ │   Client     │ │
│ │(xterm.js│ │                    │ │             │ │                    │ │              │ │
│ │         │ │                    │ │ - 会话管理   │ │                    │ │ - tmux 管理  │ │
│ └─────────┘ │                    │ │ - 认证      │ │                    │ │ - PTY 转发   │ │
│             │                    │ │ - 消息路由   │ │                    │ │ - Claude CLI │ │
│ ┌─────────┐ │                    │ └─────────────┘ │                    │ └──────────────┘ │
│ │  CLI    │ │                    │                 │                    │                  │
│ │(Termius)│ │                    │                 │                    │ ┌──────────────┐ │
│ └─────────┘ │                    │                 │                    │ │  tmux 会话   │ │
└─────────────┘                    └─────────────────┘                    │ │  ┌────────┐  │ │
                                                                          │ │  │ claude │  │ │
                                                                          │ │  └────────┘  │ │
                                                                          │ └──────────────┘ │
                                                                          └──────────────────┘
```

## 快速开始

### 1. 编译

```bash
# 克隆仓库
git clone https://github.com/shayin/claude-forward.git
cd claude-forward

# 安装依赖
make deps

# 编译
make build
```

### 2. 部署服务器

```bash
# 复制配置文件
cp configs/server.yaml /opt/claude-forward/
# 编辑配置，设置 token
vim /opt/claude-forward/server.yaml

# 启动服务器
./bin/server /opt/claude-forward/server.yaml
```

**使用 systemd 管理服务：**

```bash
# 创建服务文件
sudo cat > /etc/systemd/system/claude-forward.service << EOF
[Unit]
Description=Claude Forward Server
After=network.target

[Service]
Type=simple
User=www-data
WorkingDirectory=/opt/claude-forward
ExecStart=/opt/claude-forward/server /opt/claude-forward/server.yaml
Restart=always

[Install]
WantedBy=multi-user.target
EOF

# 启动服务
sudo systemctl daemon-reload
sudo systemctl enable claude-forward
sudo systemctl start claude-forward
```

### 3. 配置客户端

```bash
# 复制配置文件
cp configs/client.yaml ~/.claude-forward/
# 编辑配置
vim ~/.claude-forward/client.yaml
```

配置示例：

```yaml
server:
  url: "wss://your-server-ip:8765"
  token: "your-secret-token-here"
  reconnect_interval: 5

client:
  id: "home-pc"
  name: "Home Desktop"
  description: "我的开发电脑"

tmux:
  session_name: "claude-forward"
  auto_start: true
  shell: "/bin/bash"
```

### 4. 启动客户端

```bash
./bin/client -config ~/.claude-forward/client.yaml
```

### 5. 访问 Web UI

在手机浏览器中打开：

```
https://your-server-ip:8765
```

> ⚠️ 首次访问会提示证书不受信任，点击"高级" → "继续访问"即可。

## 使用说明

### Web UI

1. 打开 Web UI，输入服务器地址和 Token
2. 点击"连接"
3. 选择要连接的客户端
4. 开始使用 Claude Code

### CLI/Terminal

使用任意终端应用（如 Termius、iTerm）连接：

```bash
# 使用 wss 代理工具连接
wscat -c "wss://your-server:8765/ws?token=your-token"
```

## 配置说明

### 服务器配置 (server.yaml)

```yaml
server:
  host: "0.0.0.0"          # 监听地址
  port: 8765               # 监听端口
  tls:
    enabled: true          # 启用 TLS
    cert_file: ""          # 证书文件（留空使用自签名）
    key_file: ""           # 私钥文件
    domain: ""             # 域名（填写后使用 Let's Encrypt）

auth:
  tokens:
    - "your-secret-token"  # 访问令牌

session:
  timeout: 300             # 会话超时（秒）
  max_clients: 10          # 最大客户端数

logging:
  enabled: true
  file: "/var/log/claude-forward/server.log"
  max_days: 7
  log_level: "info"
```

### 客户端配置 (client.yaml)

```yaml
server:
  url: "wss://your-server:8765"
  token: "your-secret-token"
  reconnect_interval: 5    # 重连间隔（秒）

client:
  id: "home-pc"            # 客户端 ID（唯一）
  name: "Home Desktop"     # 显示名称
  description: ""          # 描述

tmux:
  session_name: "claude-forward"
  auto_start: true         # 自动创建 tmux 会话
  shell: "/bin/bash"       # 默认 shell
```

## 安全建议

1. **使用强 Token** - 生成足够长的随机字符串作为 Token
2. **配置防火墙** - 限制服务器端口只允许你的 IP 访问
3. **定期更新** - 保持系统和软件更新
4. **监控日志** - 定期检查访问日志

## 常见问题

### 证书警告

使用自签名证书时，浏览器会显示证书警告。这是正常的，点击"继续访问"即可。

### 无法连接

1. 检查服务器是否启动
2. 检查防火墙是否开放端口
3. 检查 Token 是否正确
4. 检查客户端是否已注册

### 终端显示异常

1. 确保本地已安装 tmux
2. 检查 tmux 会话是否正常创建

## 开发

```bash
# 运行测试
make test

# 开发模式运行服务器
make run-server

# 开发模式运行客户端
make run-client
```

## License

MIT
