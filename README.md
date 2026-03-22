# Claude Forward

 自托管的远程编程系统，让你可以通过手机或其他设备控制家中内网电脑上的 Claude Code。

## 特性

- **零成本部署** - 使用自签名证书，无需域名
- **双模式访问** - Web UI + CLI/Terminal
- **断线重连** - 集成 tmux，网络中断不会丢失会话
- **安全** - Token 认证 + TLS 加密
- **多会话** - 支持多个客户端同时连接

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

---

## 快速开始

### 前置条件

**云服务器（需要公网 IP）：**
- Linux 系统（Ubuntu/Debian/CentOS）
- 开放 6022 端口

**本地电脑（内网）：**
- Linux/macOS 系统
- 已安装 tmux
- 已安装 Claude Code CLI

---

## 第一步：编译

在云服务器上执行：

```bash
# 克隆仓库
git clone https://github.com/shayin/claude-forward.git
cd claude-forward

# 安装 Go 依赖（如果没有 Go，请先安装）
# Ubuntu: sudo apt install golang-go
# CentOS: sudo yum install golang

# 下载依赖
make deps

# 编译
make build
```

编译完成后，会在 `bin/` 目录生成：
- `bin/server` - 服务器程序
- `bin/client` - 客户端程序

---

## 第二步：部署服务器（云服务器）

### 方式一：一键部署（推荐）

```bash
sudo ./deploy/install.sh
```

脚本会引导你完成配置：
1. 输入 Token（或使用自动生成的随机 Token）
2. 输入监听端口（默认 6022）
3. 输入服务器公网 IP
4. 确认配置后自动安装

### 方式二：手动部署

```bash
# 1. 创建目录
sudo mkdir -p /opt/claude-forward
sudo mkdir -p /var/log/claude-forward

# 2. 复制文件
sudo cp bin/server /opt/claude-forward/
sudo cp bin/client /opt/claude-forward/
sudo cp -r web /opt/claude-forward/
sudo cp configs/server.yaml /opt/claude-forward/

# 3. 编辑配置（重要！修改 token）
sudo vim /opt/claude-forward/server.yaml

# 4. 设置权限
sudo useradd -r -s /bin/false claude-forward
sudo chown -R claude-forward:claude-forward /opt/claude-forward
sudo chown -R claude-forward:claude-forward /var/log/claude-forward

# 5. 安装 systemd 服务
sudo cp deploy/claude-forward-server.service /etc/systemd/system/
sudo systemctl daemon-reload

# 6. 启动服务
sudo systemctl start claude-forward-server
sudo systemctl enable claude-forward-server
```

### 验证服务器

```bash
# 查看服务状态
sudo systemctl status claude-forward-server

# 查看日志
sudo journalctl -u claude-forward-server -f
```

---

## 第三步：配置客户端（本地电脑）

### 1. 下载客户端程序

从云服务器下载客户端程序和配置：

```bash
# 创建目录
mkdir -p ~/.claude-forward

# 下载客户端程序
scp user@your-server-ip:/opt/claude-forward/client ~/.claude-forward/
chmod +x ~/.claude-forward/client

# 下载配置模板
scp user@your-server-ip:/opt/claude-forward/client.yaml.example ~/.claude-forward/client.yaml
```

### 2. 编辑配置文件

```bash
vim ~/.claude-forward/client.yaml
```

配置示例：

```yaml
server:
  url: "wss://your-server-ip:6022"    # 云服务器地址
  token: "your-secret-token-here"      # 与服务器配置的 token 一致
  reconnect_interval: 5

client:
  id: "home-pc"                        # 客户端唯一 ID
  name: "Home Desktop"                 # 显示名称
  description: "我的开发电脑"

tmux:
  session_name: "claude-forward"
  auto_start: true
  shell: "/bin/bash"
```

### 3. 启动客户端

```bash
~/.claude-forward/client -config ~/.claude-forward/client.yaml
```

### 4. 设置开机自启（可选）

创建 systemd 服务：

```bash
cat > /tmp/claude-forward-client.service << 'EOF'
[Unit]
Description=Claude Forward Client
After=network.target

[Service]
Type=simple
User=%USER%
WorkingDirectory=%HOME%/.claude-forward
ExecStart=%HOME%/.claude-forward/client -config %HOME%/.claude-forward/client.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# 替换变量
sed -i "s|%USER%|$USER|g" /tmp/claude-forward-client.service
sed -i "s|%HOME%|$HOME|g" /tmp/claude-forward-client.service

# 安装服务
sudo cp /tmp/claude-forward-client.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable claude-forward-client
sudo systemctl start claude-forward-client
```

---

## 第四步：使用 Web UI

### 1. 访问 Web 界面

在手机或电脑浏览器中打开：

```
https://your-server-ip:6022
```

> 首次访问会提示证书不受信任，点击"高级" → "继续访问"即可。

### 2. 连接服务器

1. 输入服务器地址：`wss://your-server-ip:6022`
2. 输入 Token
3. 点击"连接"

### 3. 选择客户端

连接成功后，会显示可用的客户端列表，点击要连接的客户端。

### 4. 开始使用

现在你可以在终端中使用 Claude Code 了！

---

## 配置说明

### 服务器配置 (server.yaml)

```yaml
server:
  host: "0.0.0.0"          # 监听地址（0.0.0.0 表示所有网卡）
  port: 6022               # 监听端口
  tls:
    enabled: true          # 启用 TLS（推荐）
    cert_file: ""          # 证书文件（留空使用自签名）
    key_file: ""           # 私钥文件
    domain: ""             # 域名（填写后使用 Let's Encrypt）

auth:
  tokens:
    - "your-secret-token"  # 访问令牌（请修改为强密码）

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
  url: "wss://your-server:6022"
  token: "your-secret-token"
  reconnect_interval: 5    # 重连间隔（秒）

client:
  id: "home-pc"            # 客户端 ID（必须唯一）
  name: "Home Desktop"     # 显示名称
  description: ""          # 描述

tmux:
  session_name: "claude-forward"
  auto_start: true         # 自动创建 tmux 会话
  shell: "/bin/bash"       # 默认 shell
```

---

## 常见问题

### 1. 证书警告

使用自签名证书时，浏览器会显示证书警告。这是正常的：
- Chrome: 点击"高级" → "继续访问"
- Safari: 点击"显示详细信息" → "访问此网站"

### 2. 无法连接服务器

排查步骤：
```bash
# 1. 检查服务是否运行
sudo systemctl status claude-forward-server

# 2. 检查端口是否监听
sudo netstat -tlnp | grep 6022

# 3. 检查防火墙
# Ubuntu
sudo ufw status
sudo ufw allow 6022/tcp

# CentOS
sudo firewall-cmd --list-ports
sudo firewall-cmd --add-port=6022/tcp --permanent
sudo firewall-cmd --reload

# 4. 检查云服务商安全组
# 确保在云控制台开放 6022 端口
```

### 3. 客户端无法连接

1. 检查 Token 是否正确
2. 检查服务器地址是否正确（注意是 wss:// 不是 https://）
3. 检查网络连通性

### 4. 终端显示异常

1. 确保本地已安装 tmux：`tmux -V`
2. 检查 tmux 会话：`tmux ls`
3. 尝试手动创建 tmux 会话：`tmux new -s claude-forward`

### 5. 中文输入问题

如果 Web UI 中无法输入中文，尝试：
1. 使用外接键盘
2. 使用终端 App（如 Termius）代替 Web UI

---

## 安全建议

1. **使用强 Token** - 生成足够长的随机字符串（建议 32 位以上）
2. **配置防火墙** - 限制端口只允许你的 IP 访问
3. **定期更新** - 保持系统和软件更新
4. **监控日志** - 定期检查访问日志

```bash
# 限制只允许特定 IP 访问
# Ubuntu
sudo ufw delete allow 6022/tcp
sudo ufw allow from YOUR_IP to any port 6022 proto tcp

# CentOS
sudo firewall-cmd --remove-port=6022/tcp --permanent
sudo firewall-cmd --add-rich-rule='rule family="ipv4" source address="YOUR_IP" port protocol="tcp" port="6022" accept' --permanent
sudo firewall-cmd --reload
```

---

## 开发模式：快速启动脚本

如果你想在开发环境中快速启动服务器或客户端，可以使用提供的启动脚本。

### 服务器启动脚本

```bash
# 启动服务器（自动编译）
./start-server.sh start

# 重启服务器（重新编译）
./start-server.sh restart

# 停止服务器
./start-server.sh stop

# 查看状态
./start-server.sh status

# 查看日志
./start-server.sh logs
```

### 客户端启动脚本

```bash
# 启动客户端（自动编译）
./start-client.sh start

# 重启客户端（重新编译）
./start-client.sh restart

# 停止客户端
./start-client.sh stop

# 查看状态
./start-client.sh status

# 查看日志
./start-client.sh logs
```

> 脚本会在后台运行进程，PID 文件存放在 `/tmp/` 目录下。

---

## 开发

```bash
# 运行测试
make test

# 开发模式运行服务器（前台）
make run-server

# 开发模式运行客户端（前台）
make run-client

# 生成自签名证书
make gen-cert
```

---

## 项目结构

```
claude-forward/
├── cmd/
│   ├── server/main.go      # 服务器入口
│   └── client/main.go      # 客户端入口
├── internal/
│   ├── protocol/           # 消息协议
│   ├── server/             # 服务器逻辑
│   └── client/             # 客户端逻辑
├── configs/
│   ├── server.yaml         # 服务器配置模板
│   └── client.yaml         # 客户端配置模板
├── deploy/
│   ├── install.sh          # 一键部署脚本
│   └── *.service           # systemd 服务文件
├── web/
│   └── index.html          # Web UI
├── start-server.sh         # 服务器启动脚本
├── start-client.sh         # 客户端启动脚本
├── Makefile
└── README.md
```

---

## License

MIT
