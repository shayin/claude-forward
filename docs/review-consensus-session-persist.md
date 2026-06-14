# Client 重启 Bot Session 持久化 — GLM × DS 讨论共识

**日期**: 2026-06-14
**参与方**: GLM (`docs/review-glm-session-persist.md`) × DS (`docs/review-ds-session-persist.md`)
**讨论方式**: 双向监听对方文件，一问一答
**实现归属**: A（GLM 窗口）实施

## 状态
**最终共识已达成 ✅**（R1-R3）

---

## 一、议题

每次重启客户端，微信端（Bot API）的 Claude session 丢失，无法 `--resume` 续接之前的对话。

---

## 二、根因

**Bot API 的 `botSessionID` 只存在内存里，没有持久化。**

- `connection.go:45` `botSessionID string` — 纯内存字段
- `connection.go:655` 事件中收到 session_id 只更新内存
- `connection.go:577` Client 重启后 botSessionID 为空 → 不传 `--resume` → Claude 启动全新会话
- 对比：Web UI 走 `ClaudeManager`（`claude.go`）已有完整持久化（`saveSessionID`/`loadSessionID`）

**额外发现**：`botSessionID` 在 `handleMessage` 和 `handleChatInput` 之间无锁，存在 data race。

---

## 三、最终共识

### 3.1 方案

**把 `botSessionID` 字段从 Client 移到 ClaudeManager**，复用已有的 save/load 机制，用独立的 bot session 文件保持与 Web UI 隔离。

### 3.2 为什么隔离

Web UI 和 Bot API 是**不同的对话上下文**。共用 session ID 会导致 Claude 串上下文。隔离不是历史包袱，是功能正确性要求。

### 3.3 实现要点

**ClaudeManager 新增**（`claude.go`）：
- `botSessionID string` 字段
- `botSessionIDPath() string` — 返回 `~/.claude-forward/session_id_bot[_<clientID>]`
- `BotSessionID() string` — 带锁读取
- `SetBotSessionID(id string)` — 带锁写入 + 调用 `saveBotSessionID()`
- `saveBotSessionID()` / `loadBotSessionID()` — 与现有 `saveSessionID`/`loadSessionID` 同模式，只路径不同
- `NewClaudeManager` 中调用 `loadBotSessionID()` 恢复

**Connection 层改动**（`connection.go`）：
- 删除 `c.botSessionID` 字段
- `handleMessage` /new 处理：`c.claude.SetBotSessionID("")`
- `handleChatInput` 设置：`c.claude.SetBotSessionID(event.SessionID)`
- `handleChatInput` 读取：`c.claude.BotSessionID()`

### 3.4 收益

- 持久化：Client 重启后 bot session 恢复
- Data race 解决：`cm.mu` 天然保护
- 代码复用：不重复 save/load 逻辑
- 结构清晰：Client 少一个字段

---

## 四、讨论时间线

| Round | 方向 | 主题 | 结果 |
|-------|------|------|------|
| R1 | GLM → DS | 根因定位 + 方案 A/B 选择 | 倾向 B（独立持久化）|
| R1 | DS → GLM | 同意 B，提变体（ClaudeManager 加方法）+ data race 发现 | 变体更优 |
| R2 | GLM → DS | 接受变体，建议字段移到 ClaudeManager | 抛反问 |
| R2 | DS → GLM | 全部同意，列实现要点 | 共识达成 |
| R3 | GLM → DS | 收尾确认 | 讨论结束 |
