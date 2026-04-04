<h1 align="center">Claude Forward</h1>

<p align="center">
<strong>自托管远程编程系统 — 用手机控制电脑上的 Claude Code</strong>
</p>

<p align="center">
手机端通过云服务器中转，远程控制本地电脑运行的 Claude Code。<br>
随时随地写代码。
</p>

<p align="center">
<a href="#快速开始">快速开始</a> · <a href="#配置说明">配置说明</a> · <a href="#权限审批">权限审批</a> · <a href="#常见问题">常见问题</a>
</p>

---

## 它解决什么问题

你在电脑上用 Claude Code 写代码，但不可能一直坐在电脑前。Claude Forward 让你用手机浏览器就能：

- 给 Claude 发消息，看实时流式回复
- 审批 Claude 的工具调用（文件读写、命令执行等）
- 断开重连不丢失上下文，自动恢复会话

**架构简单，你完全掌控：**

```
手机浏览器 ◄── WebSocket ──► 云服务器 ◄── WebSocket ──► 你的电脑 (Claude Code)
              (TLS 加密)        (只做转发)         (实际运行 Claude)
```

云服务器只转发消息，不运行 Claude，不存储对话。你的代码和数据始终在你自己的机器上。

## 功能

| 功能 | 说明 |
|------|------|
| 聊天界面 | 仿 Claude.ai 风格，Markdown 渲染、流式输出、工具卡片 |
| 权限审批 | Claude 执行工具时手机端弹窗，你批准或拒绝 |
| 会话恢复 | 断线重连自动恢复，支持 Claude `--resume` |
| 费用统计 | 实时显示每轮 Token 用量 |
| 终端模式 | tmux + xterm.js 完整终端转发（可选） |
| 手机优化 | 适配 iOS Safari，安全区域适配 |
| 安全 | Token 认证 + TLS 加密 + 自签名证书（无需域名） |

## 快速开始

### 前提条件

- 一台有公网 IP 的云服务器（用于消息中转）
- 你的开发电脑（运行 Claude Code）
- 已安装 [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) 并配置好 API Key
- Go 1.21+

### 1. 编译

```bash
git clone https://github.com/shayin/claude-forward.git
cd claude-forward
make deps
make build
```

生成两个二进制：
- `bin/server` → 部署到云服务器
- `bin/client` → 部署到你的电脑

### 2. 部署服务器（云服务器）

将编译产物上传到云服务器：

```bash
scp -r bin/ web/ configs/ deploy/ user@your-server:~/claude-forward/
```

SSH 到云服务器，运行一键部署：

```bash
cd ~/claude-forward
sudo bash deploy/install.sh
```

脚本会引导你配置 Token 和端口，自动安装 systemd 服务。完成后：

```bash
sudo systemctl start claude-forward-server   # 启动
sudo systemctl enable claude-forward-server  # 开机自启
```

### 3. 启动客户端（你的电脑）

从云服务器下载客户端和配置文件：

```bash
mkdir -p ~/.claude-forward

# 下载客户端
scp user@your-server:/opt/claude-forward/client ~/.claude-forward/
chmod +x ~/.claude-forward/client

# 下载配置模板
scp user@your-server:/opt/claude-forward/client.yaml.example ~/.claude-forward/client.yaml
```

编辑 `~/.claude-forward/client.yaml`，填入你的服务器地址和 Token：

```yaml
server:
  url: "wss://your-server-ip:6022"
  token: "your-secret-token"
```

启动客户端：

```bash
~/.claude-forward/client -config ~/.claude-forward/client.yaml
```

### 4. 打开手机浏览器

访问 `https://your-server-ip:6022`，输入 Token 登录，开始使用。

> 首次访问会提示证书不受信任（自签名证书），点击 "高级" → "继续访问" 即可。

## 配置说明

### 服务器配置 (`server.yaml`)

```yaml
server:
  host: "0.0.0.0"
  port: 6022
  tls:
    enabled: true
    cert_file: ""           # 留空自动生成自签名证书
    key_file: ""
    domain: ""              # 有域名可配置 Let's Encrypt

auth:
  tokens:
    - "your-secret-token"   # 支持多个 Token

session:
  timeout: 300              # 会话超时（秒）
  max_clients: 10           # 最大客户端数

logging:
  enabled: true
  file: "/var/log/claude-forward/server.log"
  max_days: 7
  log_level: "info"
```

### 客户端配置 (`client.yaml`)

```yaml
server:
  url: "wss://your-server-ip:6022"
  token: "your-secret-token"
  reconnect_interval: 5     # 重连间隔（秒）

client:
  id: "home-pc"             # 客户端标识，留空自动生成
  name: "Home Desktop"      # 显示名称

tmux:
  session_name: "claude-forward"
  auto_start: true
  shell: "/bin/bash"
```

## 权限审批

Claude Code 执行敏感操作（写文件、运行命令等）时需要你批准。Claude Forward 通过 [PreToolUse Hook](https://docs.anthropic.com/en/docs/claude-code/hooks) 实现远程审批：

```
Claude 要执行工具
  → 匹配 allow 规则？ → 自动批准
  → 匹配 deny 规则？ → 自动拒绝
  → 匹配 ask 规则？  → 推送到你的手机 → 你决定批准/拒绝
```

规则配置在 `~/.claude/settings.json`：

```json
{
  "permissions": {
    "allow": ["Bash(ls)", "Read"],
    "deny": ["Bash(rm -rf *)"],
    "ask": ["Bash", "Write", "Edit"]
  }
}
```

无需任何额外配置，客户端启动时自动注册 Hook。

## 项目结构

```
claude-forward/
├── cmd/
│   ├── server/main.go       # 服务器入口
│   └── client/main.go       # 客户端入口
├── internal/
│   ├── protocol/message.go  # 消息协议
│   ├── server/              # 服务器：连接管理、消息路由、认证
│   └── client/              # 客户端：Claude 交互、Hook 服务、权限解析
├── web/
│   └── index.html           # Web UI（单文件 SPA）
├── configs/                 # 配置模板
├── deploy/                  # 一键部署脚本 + systemd 服务
└── Makefile
```

## 开发

```bash
make deps          # 安装依赖
make build         # 编译
make run-server    # 开发模式运行服务器
make run-client    # 开发模式运行客户端
make gen-cert      # 生成自签名证书
make test          # 运行测试
```

## 常见问题

**Q: 需要域名吗？**
不需要。直接用 IP + 端口访问。有域名可以在配置中启用 Let's Encrypt。

**Q: 支持哪些平台？**
Server: Linux。Client: macOS / Linux。Web UI: 所有现代浏览器（手机和电脑）。

**Q: 数据安全吗？**
云服务器只做消息转发，不存储对话内容。通信全程 TLS 加密。Claude Code 在你的电脑上运行，代码和数据不离开你的机器。

**Q: 手机锁屏后重连会丢消息吗？**
不会。Client 端缓存消息，重连后自动恢复。会话记录持久化在浏览器 localStorage 中。

**Q: 连不上服务器？**
1. 检查服务状态：`sudo systemctl status claude-forward-server`
2. 检查端口监听：`sudo netstat -tlnp | grep 6022`
3. 检查防火墙是否放行端口
4. 检查云服务商安全组是否开放端口

## License

MIT
