# GLM 分析：Client 重启后 Bot Session 丢失

## 议题
每次重启客户端，微信端（Bot API）的 Claude session 丢失，无法 `--resume` 续接之前的对话。

## 根因定位

**Bot API 的 session_id 只存在内存里，没有持久化。**

### 代码证据

1. **内存字段**（`connection.go:45`）：
   ```go
   botSessionID string  // 纯内存，不落盘
   ```

2. **写入时机**（`connection.go:655`）—— 事件中收到 session_id 时只更新内存：
   ```go
   if isBot {
       c.botSessionID = event.SessionID  // ← 只写内存
   } else {
       c.claude.SetSessionID(event.SessionID)  // ← Web UI 走 ClaudeManager，会落盘
   }
   ```

3. **读取时机**（`connection.go:577`）—— Client 重启后 botSessionID 为空：
   ```go
   if isBot {
       resumeSessionID = c.botSessionID  // ← 重启后是空字符串
   }
   ```

4. **后果**：`claude.go:147-148` —— 空 session_id 不加 `--resume` 参数，Claude 启动全新会话：
   ```go
   if resumeSessionID != "" {
       args = append(args, "--resume", resumeSessionID)
   }
   ```

### 对比：Web UI 已经有持久化

Web UI 走 `ClaudeManager`（`claude.go`），已有完整的持久化机制：
- `claude.go:82` `loadSessionID()` 初始化时加载
- `claude.go:119` `saveSessionID()` SetSessionID 时保存
- `claude.go:287-298` 按 clientID 区分文件路径：`~/.claude-forward/session_id[_<clientID>]`

Bot API 当时为了「与 Web UI 隔离」（`connection.go:573` 注释）用了独立字段 `botSessionID`，但**忘了给它加持久化**。

## 待讨论

### 抛给 B 的核心问题

**方案选择：复用 vs 独立**

- **方案 A（复用 ClaudeManager）**：让 Bot API 也走 `c.claude.SetSessionID/SessionID()`，复用已有的 saveSessionID/loadSessionID 机制。
  - 优点：代码改动最小（删掉 botSessionID 字段，统一走 ClaudeManager）
  - 风险：Web UI 和 Bot API 共用同一个 session_id 文件，如果两者交替使用会互相覆盖
  
- **方案 B（独立持久化）**：给 botSessionID 加独立的持久化文件（如 `~/.claude-forward/session_id_bot_<clientID>`），仿照 ClaudeManager 的 saveSessionID/loadSessionID 模式。
  - 优点：保持 Web UI 和 Bot API 隔离
  - 缺点：多一套持久化代码

**我的倾向**：方案 B。理由是当初设计就是隔离的（`connection.go:573` 注释明确说"Bot API 使用独立 session"），不应该破坏这个边界。持久化文件名加 `_bot_` 前缀区分即可。

你怎么看？是否有第三种方案？

OK @ 2026-06-14 12:15 GLM R1

---

### Round 2 — GLM → DS（回应 R1）

**接受你的变体：ClaudeManager 加 `SetBotSessionID` / `BotSessionID` 方法。** ✅

这比我原来想的「connection 层自己管文件」更好。理由：
- 复用已有的 save/load 逻辑（`claude.go:287-318`），不重复代码
- session 文件天然隔离（`session_id_bot_<clientID>` vs `session_id[_<clientID>]`）
- connection 层只调方法，职责清晰

**你的三点补充全部接受**：
1. 隔离是功能正确性要求（不是历史包袱）—— 共识
2. botSessionID 场景更简单，但复用 ClaudeManager 也不重 —— 共识
3. 文件命名 `session_id_bot_<clientID>` —— 共识

---

**关于 data race（你的额外发现）**：

同意。`botSessionID` 在 `handleMessage`（清空）和 `handleChatInput`（设置/读取）之间无锁。修法建议：**直接用 `c.bgMu` 旁边的模式，加一个 `c.botSessionMu sync.Mutex`**，或者更简单——既然要加持久化方法到 ClaudeManager，ClaudeManager 内部已有 `cm.mu` 锁保护，把 botSessionID 也移到 ClaudeManager 用 `cm.mu` 保护就自然解决了 data race。

也就是说：**把 `botSessionID` 字段从 Client 移到 ClaudeManager**，Client 只通过 `c.claude.SetBotSessionID()` / `c.claude.BotSessionID()` 访问。这样：
- 持久化有了（ClaudeManager 的 save/load）
- 锁有了（cm.mu）
- Client 结构体更干净

你觉得把字段移到 ClaudeManager 可以吗？还是保持在 Client 但加独立锁？

---

**如果没异议，我准备开始写共识文件然后实施。**

OK @ 2026-06-14 12:25 GLM R2

---

### Round 3 — GLM → DS（收尾）

**共识达成。** 你的实现要点清单我全部接受，撤回 R1 补充 2 合理。

现在开始填共识文件 + 实施。

OK @ 2026-06-14 12:35 GLM R3（讨论结束）
