# Claude Forward

自托管的远程编程系统：通过微信/飞书消息控制内网电脑上的 Claude Code。

## 架构

```
手机(微信/飞书) ←→ 云服务器(Server) ←→ 本地电脑(Client/cf)
```

- **Server**（腾讯云 Ubuntu，`43.163.223.4:6022`）：消息中转 + 内置微信/飞书模块
- **Client**（本机 Mac，`cf` 命令）：运行 Claude Code，注册到 Server
- 两台服务器上的 client（ai-wiki、claude-forward）走飞书；本机 client（mind）走微信

## 微信通道的两种实现（重要！）

### 当前在用：Server 内置 wechat 模块

`internal/server/wechat.go` — Server 进程内直接处理微信消息/推送，功能最完整。

- Session 文件：`wechat-data/{index}/session.json`
- 路由配置：`configs/server.yaml` 的 `wechat.users` 列表
- Push API：`POST /api/wechat/push`（Bearer push_secret 认证）

### 已废弃但保留：wechat-bridge

`cmd/wechat-bridge/` — 独立进程，扫码登录 + 消息路由。

- **功能上已被 Server 内置版取代**（消息路由不再需要）
- **但它的扫码登录流程比 Server 的 relogin 更靠谱**（2026-08-23 踩坑验证）：
  - bridge 扫码 → 生成终端二维码 → 用户扫 → 保存全新 bot session → 直接绑定用户微信
  - Server 的 `POST /api/wechat/relogin/{index}` 反复出问题（生成新 bot 但用户微信绑的还是旧的，消息收不到）
- Session 文件：`configs/wechat-session.json`

### Session 过期时的恢复方法

1. 在本地跑 `wechat-bridge`（需要 `configs/wechat-bridge.yaml` 配好 server 地址和 token）
2. 终端会显示二维码，用微信扫
3. 成功后把 `configs/wechat-session.json` 拷贝到服务器的 `wechat-data/0/session.json`
4. 重启 Server（`tmux kill-session -t cf-server; tmux new-session -d -s cf-server 'cd ~/claude-forward && ./bin/server configs/server.yaml > /tmp/claude-forward-server.log 2>&1'`）

> ⚠️ 不要用 Server 的 `relogin` API — 它会生成新 bot 但用户微信绑定不变，导致消息失联。
> ⚠️ 不要调 `GET /api/wechat/qrcode/{index}` — 也会触发 relogin 流程，副作用同上。

## Server 运维

```bash
# 服务器上的操作（SSH: ubuntu@43.163.223.4, 密钥: ~/.ssh/tencent_aermac）

# 查看日志
tail -f /tmp/claude-forward-server.log

# 重启
pkill -9 -f bin/server; sleep 2
tmux new-session -d -s cf-server 'cd ~/claude-forward && ./bin/server configs/server.yaml > /tmp/claude-forward-server.log 2>&1'

# 编译
cd ~/claude-forward && /usr/local/go/bin/go build -o bin/server ./cmd/server
```

## Client 启动（本机 Mac）

```bash
# mind 项目（微信）
cd ~/data1/htdocs/project/mind && cf    # 前台运行，Ctrl+C 停止

# 注册名: xuanxindeMacBook-Pro.local-mind（hostname-目录名）
# clawbot_id: xuanxindeMacBook-Pro.local（hostname）
```

## 关键 ID 对应关系

| 标识 | 值 | 说明 |
|---|---|---|
| 本机 hostname | xuanxindeMacBook-Pro.local | clawbot_id 用这个 |
| cf client ID（mind） | xuanxindeMacBook-Pro.local-mind | Server 注册时的 ID |
| 我的微信 ID | o9cq800CZjb8XStvFWGnLAhD7_s4@im.wechat | Server wechat.users[0] |
| Server 管理Token | configs/server.yaml 里的 tokens | API 调用用 |
| Push Secret | daxing1992 | push API 的 push_secret |

## 注意事项

- Server 修改代码后需要重新编译部署：`go build -o bin/server ./cmd/server` 然后 kill + 重启
- Client 断线会自动重连，但 tmux 会话冲突时会退出（报 "already exists"），需要 `tmux kill-session -t cf-mind` 后重新启动
- 微信 bot session 过期症状：推送返回 `queued` 而非 `sent`；消息对话无回复。用 bridge 扫码流程恢复（见上方）
