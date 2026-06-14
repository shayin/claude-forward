# 后台模式超时 Bug 审查报告

**日期**: 2026-06-06
**审查范围**: 后台模式 / 超时 / 任务生命周期代码
**审查文件**: 
- `internal/server/wechat.go` (chatViaHub)
- `internal/server/hub.go` (Hub, Unregister, handleChatClientToUser)
- `internal/server/handler.go` (handleBackgroundResult, handleChatClientToUser)
- `internal/client/connection.go` (handleChatInput, handleMessage)
- `internal/client/claude.go` (ClaudeManager)

---

## 报告摘要

发现 **3 个严重问题**、**3 个中等严重问题**、**2 个轻微问题**。

核心根因：当 Client 断线重连时，`Hub.Unregister` 会清空该 Client 的所有 `attachMap` 条目，导致正在运行的 `chatViaHub` 失去与 Client 事件的联系。`chatViaHub` 无法收到任何事件，3 分钟 timer 必然触发，返回 `IsBackground: true`。同时，`handleChatClientToUser` 的「只发送给第一个匹配用户」设计，在存在多个 bot 连接时（如延迟清理窗口期内）会导致事件路由到错误（已废弃）的 botConn。

---

## 严重问题

### 问题 1：Client 断线重连导致活跃的 chatViaHub 失去事件路由

**严重等级**: 严重
**文件**: `internal/server/hub.go` 第 69-90 行
**影响**: 用户报告的核心症状——每次发消息都触发「任务已超时转后台运行」

**详细说明**:

当 Client 断线（WebSocket 连接断开，如网络波动、Mac 休眠/唤醒、Server 重启）时，`Hub.Run()` 中的 `unregister` 分支会执行：

```go
// hub.go:69-90
case conn := <-h.unregister:
    h.mu.Lock()
    if conn.Type == ConnTypeClient {
        if existing, ok := h.clients[conn.ID]; ok && existing == conn {
            delete(h.clients, conn.ID)
            // 通知所有附加到此客户端的用户
            for userID, clientID := range h.attachMap {
                if clientID == conn.ID {
                    // ... 发送 TypeDetached
                    delete(h.attachMap, userID)  // <-- 清空所有 attachMap
                }
            }
        }
    }
```

这段代码会删除该 Client 在 `h.attachMap` 中的**所有**条目。包括 `chatViaHub` 创建的 botConn 条目。

当 `h.attachMap` 中不再有该 Client 的条目后，`handleChatClientToUser`（handler.go 第 466-482 行）无法找到目标用户，Client 发来的事件被静默丢弃：

```go
// handler.go:466-482
func (h *Handler) handleChatClientToUser(conn *Connection, msg *protocol.Message) {
    h.hub.mu.RLock()
    var targetUser *Connection
    for userID, clientID := range h.hub.attachMap {
        if clientID == conn.ID {
            if user, ok := h.hub.users[userID]; ok {
                targetUser = user
                break
            }
        }
    }
    h.hub.mu.RUnlock()
    if targetUser != nil {
        targetUser.Send <- msg
    }
    // 如果 targetUser == nil（attachMap 为空），消息被丢弃
}
```

**后果链**:
1. Client 断线（如 Mac 休眠）
2. Client 重连（自动重连机制），重新注册到 Hub
3. 旧的 `chatViaHub`（botConn-1）仍然在等待事件，但 `h.attachMap` 已被清空
4. Claude 进程在 Client 端继续运行，事件通过新连接到达 Server
5. `handleChatClientToUser` 找不到任何 user → 事件全部丢弃
6. `chatViaHub` 收不到事件，3 分钟 timer 到期 → 返回 `IsBackground: true`
7. WeChat 收到「任务已超时转后台运行」→ 发送给用户
8. Claude 实际在实时处理（因为 Client 端的 handleChatInput goroutine 还在跑）
9. 任务完成后通过 `TypeBackgroundResult` 推送结果 → 用户看到「实时返回」

**为何"到早上"才出现**: 
Mac 在夜间休眠唤醒后，Client 会自动重连。此时如果有一个长时间运行的 Claude 任务在后台执行，重连会导致 chatViaHub 的 attachMap 被清空。后续每个新消息都会走同样的路径：新的 chatViaHub 被创建，attachMap 中只有当前 botConn（旧 botConn 在 10 秒后被清理），事件本应正常路由。但当 Client 网络不稳定（频繁断连重连）时，症状会反复出现。

**为何"每句话"都触发**: 
如果 Client 反复断连重连（例如因网络不稳定导致 30 秒读超时），每次重连都会清空 attachMap，使得正在进行的 chatViaHub 超时。每个新消息都重复这个循环。

---

### 问题 2：handleChatClientToUser 只发送给单个用户，多 botConn 场景下事件路由错误

**严重等级**: 严重
**文件**: `internal/server/handler.go` 第 466-482 行
**影响**: 多个 bot 连接同时存在时，事件可能被路由到已废弃的 botConn，导致活跃 botConn 的 chatViaHub 饿死超时

**详细说明**:

```go
// handler.go:466-482
func (h *Handler) handleChatClientToUser(conn *Connection, msg *protocol.Message) {
    h.hub.mu.RLock()
    var targetUser *Connection
    for userID, clientID := range h.hub.attachMap {
        if clientID == conn.ID {
            if user, ok := h.hub.users[userID]; ok {
                targetUser = user
                break  // <-- 只取第一个匹配，其余忽略
            }
        }
    }
    h.hub.mu.RUnlock()
    if targetUser != nil {
        targetUser.Send <- msg  // 只有一个 user 收到
    }
}
```

当 `h.attachMap` 中存在两个或更多的 botConn 映射到同一个 `clientID` 时，此函数只将事件发送给**迭代顺序碰到的第一个**。Go map 的迭代顺序是随机的，因此事件可能随机路由到任意一个 botConn。

**触发场景**:
1. 10 秒延迟清理窗口期（见问题 3）
2. WeChat 批量拉取到多条消息时，`pollLoop` 并发启动多个 `handleMessage` goroutine，每个都会调用 `chatViaHub` 创建新的 botConn

**后果**:
- 如果事件被路由到已超时/已返回的旧 botConn → 无人读取 → 新 botConn 的 chatViaHub 饿死 → 3 分钟 timer 触发 →「转后台」
- 如果用户的错误消息（"Claude is still processing"）被路由到错误的 botConn → 新 botConn 收不到 → 3 分钟 timer 触发

---

### 问题 3：10 秒延迟清理造成 botConn 残留窗口

**严重等级**: 严重
**文件**: `internal/server/wechat.go` 第 400-412 行
**影响**: 超时转后台后 10 秒内，旧 botConn 仍在 `h.attachMap` 中，与问题 2 叠加导致新消息的事件路由错误

**详细说明**:

```go
// wechat.go:400-412
defer func() {
    if wentBackground {
        // 延迟 10 秒清理
        go func() {
            time.Sleep(10 * time.Second)
            m.hub.DetachUser(botConn.ID)
            m.hub.CleanupBotUser(botConn)
        }()
    } else {
        // 正常路径：立即清理
        m.hub.DetachUser(botConn.ID)
        m.hub.CleanupBotUser(botConn)
    }
}()
```

这 10 秒延迟的目的是让 Client 有时间收到 `TypeBackgroundMode` 消息。但它同时创建了一个 10 秒的窗口期，在此期间：
- 旧 botConn 仍在 `h.attachMap` 中
- 新消息到达时会创建新的 botConn（也加入 `h.attachMap`）
- 两个 botConn 都在 `h.attachMap` 中映射到同一个 `clientID`

与问题 2 结合：新 botConn 的 chatViaHub 有约 50% 概率收不到事件 → 超时 →「转后台」。

---

## 中等严重问题

### 问题 4：后台超时后仍发送 TypeDetach 给 Client

**严重等级**: 中等
**文件**: `internal/server/wechat.go` 第 435-439 行
**影响**: Client 端 `attachedUser` 被错误清空，可能影响并发的消息处理

**详细说明**:

```go
// wechat.go:435-439
defer func() {
    safeSend(client.Send, &protocol.Message{
        Type: protocol.TypeDetach,
        From: botConn.ID,
    })
}()
```

当 `chatViaHub` 超时走后台模式时（`wentBackground = true`），这个 defer 仍然执行，向 Client 发送 `TypeDetach`。Client 处理后调用 `c.setUser("")`，清空 `attachedUser`。

这在语义上是错误的：后台任务还在继续运行，但 Client 端已经认为 bot 用户断开了。虽然后台模式的文本收集不依赖 `attachedUser`（通过 `bgActive` 跳过发送），但与其他并发操作（如同时到达的新消息的 TypeAttach）之间可能存在竞态。

---

### 问题 5：自动后台检测设置错误的 WechatID

**严重等级**: 中等
**文件**: `internal/client/connection.go` 第 699-707 行
**影响**: 断线自动切换后台时，结果推送可能发送到无效的 WeChat 用户

**详细说明**:

```go
// connection.go:699-707
c.bgMu.Lock()
bgActive = c.bgMode
if !bgActive && atomic.LoadInt64(&c.connGen) != startGen {
    c.bgMode = true
    c.bgTaskID = fmt.Sprintf("auto-bg-%d", time.Now().UnixMilli())
    c.bgWechatID = strings.TrimPrefix(userID, "bot-")  // BUG: 错误地使用 bot 连接 ID
    bgActive = true
}
c.bgMu.Unlock()
```

当 Client 检测到 `connGen` 变化（断线重连）自动切换后台时，`c.bgWechatID` 被设置为 `strings.TrimPrefix(userID, "bot-")`。这里的 `userID` 是 `chatViaHub` 创建的虚拟 botConn 的 ID（如 `"bot-wechat-<uuid>"`），而不是实际的微信用户 ID。

正确的 WeChat ID 应由 Server 通过 `TypeBackgroundMode` 消息中的 `BackgroundModePayload.WechatID` 字段传递。但在自动检测路径中，这条消息从未被发送，因此使用了错误的 ID。

**后果**: 任务完成后，`TypeBackgroundResult` 中的 `WechatID` 是 `"wechat-<uuid>"`，`PushMessage` 无法找到对应的微信用户，推送失败。

---

### 问题 6：chatViaHub 的 timer 未对所有事件类型 reset

**严重等级**: 中等
**文件**: `internal/server/wechat.go` 第 457-495 行
**影响**: 某些事件类型到达时不会重置 3 分钟超时，可能导致误判超时

**详细说明**:

在 `chatViaHub` 的事件循环中：

```go
switch msg.Type {
case protocol.TypeChatMessage:
    switch payload.EventType {
    case "stream_delta":
        timeout.Reset(3 * time.Minute)  // 只有这个 reset
    case "text":
        timeout.Reset(3 * time.Minute)  // 只有这个 reset
    case "result":
        // 未 reset timer
    case "tool_start":
        // 未 reset timer
    }
// TypeChatAck 未处理，也未 reset timer
}
```

以下事件类型到达时不会重置超时计时器：
- `TypeChatAck`（Client 确认收到聊天消息）
- `ChatMessage` 的 `"result"` 和 `"tool_start"` 子类型

如果 Claude 长时间执行工具调用（如超过 3 分钟的大文件读取），期间只有 `tool_start` 而没有 `stream_delta`/`text`，timer 不会被重置，会触发超时。

---

## 轻微问题

### 问题 7：botConn.Send channel 永不关闭

**严重等级**: 轻微
**文件**: `internal/server/hub.go` 第 127-135 行（CleanupBotUser）
**影响**: 潜在的 goroutine 泄漏（当前代码中不构成实际问题，因为 chatViaHub 是同步调用且 botConn.Send 仅由 chatViaHub 读取）

**详细说明**:

`CleanupBotUser` 只从 map 中删除 botConn，不调用 `close(conn.Send)`。而 `Unregister`（hub.go 第 109 行）会 `close(conn.Send)`。botConn 走的是 `RegisterBotUser`（直接操作 map）→ `CleanupBotUser`（直接操作 map）路径，跳过了 `Unregister` → `close(conn.Send)` 步骤。

当前代码中不会导致实际问题（chatViaHub 会在 cleanup 之前退出事件循环），但从资源管理的角度是一个不一致点。

---

### 问题 8：chatViaHub 返回后到延迟清理之间，TypeDetach 与 TypeBackgroundMode 的处理顺序

**严重等级**: 轻微
**文件**: `internal/server/wechat.go` 第 498-517 行（发送 TypeBackgroundMode）及第 435-439 行（defer 发送 TypeDetach）
**影响**: Client 端 handleChatInput goroutine 可能先看到 TypeDetach 的效果，但 handleMessage goroutine 处理 TypeBackgroundMode 才设置 bgMode。不过 handleChatInput 的 10 秒轮询等待（connection.go 第 822-830 行）已妥善处理此竞态。

**详细说明**:

`chatViaHub` 在 function body 中发送 `TypeBackgroundMode`：
```go
safeSend(client.Send, bgMsg)
```

然后在 defer 中发送 `TypeDetach`：
```go
safeSend(client.Send, &protocol.Message{Type: protocol.TypeDetach, From: botConn.ID})
```

这两条消息按正确顺序到达 Client（TypeBackgroundMode 先，TypeDetach 后）。Client 的 `handleChatInput` 有 10 秒的轮询等待 `bgMode` 设置，已正确处理此竞态。此问题仅为文档性/可读性问题。

---

## 事件流程图（正常路径 vs 断线重连路径）

### 正常路径
```
WeChat → pollLoop → handleMessage → chatViaHub
  ↓
  创建 botConn → RegisterBotUser → AttachUser
  ↓
  发送 TypeAttach + TypeChatInput → Client
  ↓
  Client.handleChatInput → Claude 进程
  ↓
  events → c.send → WS → Server → handleChatClientToUser → botConn.Send
  ↓
  chatViaHub 收到事件 → reset timer → 累加文本
  ↓
  TypeChatReady → chatViaHub 返回 → 发送完整回复到 WeChat
```

### 断线重连导致超时路径
```
WeChat → chatViaHub → 创建 botConn-1 → 事件开始流动
  ↓
  Client 断线（网络/Mac休眠）
  ↓
  Hub.Unregister → delete all attachMap entries → botConn-1 失去路由
  ↓
  Client 重连 → 重新注册
  ↓
  Claude 事件流继续 → Server.handleChatClientToUser → 找不到任何 user → 事件丢弃
  ↓
  chatViaHub 的 3min timer 触发 → wentBackground=true → 发送 TypeBackgroundMode
  ↓
  WeChat 收到: "已转为后台运行"   ← 用户看到的错误提示
  ↓
  Client 端 handleChatInput 继续运行（auto-bg detected）→ 任务完成
  ↓
  TypeBackgroundResult → 推送到 WeChat   ← "实际上任务都是实时返回的"
```

---

## 修复建议（概要，不做代码修改）

### 针对问题 1（最优先修复）：
- `Unregister` 在清理 `attachMap` 时，应跳过 bot 连接（ID 以 "bot-" 开头的条目），或改为仅删除非 bot 条目
- 或：在 `handleChatClientToUser` 中，如果 `h.attachMap` 中找不到 user，应检查 `h.users` 中是否有 bot 连接仍然存在（bot 连接同时在 `h.users` 中），将事件发送给所有匹配的 user（而不仅仅是第一个）
- 或：`chatViaHub` 在检测到事件流中断时（通过额外的超时机制），主动检查 Client 是否仍然在线

### 针对问题 2：
- 将 `handleChatClientToUser` 改为向所有匹配的 user 发送事件（broadcast），而不是只发给第一个
- 或：确保同一时间每个 Client 只有一个 bot 连接（在创建新 botConn 前先清理旧的）

### 针对问题 3：
- 将 10 秒延迟清理改为条件清理：在清理之前检查是否已有新的 bot 连接创建
- 或：在创建新 botConn 时立即清理旧的 botConn（在 `chatViaHub` 开头主动清理旧条目）

### 针对问题 5：
- 将正确的 WeChat ID 保存在 Client 的某个持久化字段中（由 Server 的 TypeBackgroundMode 消息传递）
- 或在自动检测路径中，不设置 WechatID，改为在 TypeBackgroundResult 中包含 taskID，由 Server 端根据 taskID 查找

### 针对问题 6：
- 在 `"result"` 和 `"tool_start"` 事件分支中添加 `timeout.Reset(3 * time.Minute)`
- 为 `TypeChatAck` 添加显式的 timer reset

---

## 与 GLM 报告讨论（一问一答）

> 对方文件：`docs/review-glm-timeout-bg.md`
> 共识文件：`docs/review-consensus-2026-06-14.md`

### Round 1 — GLM → DS

**GLM 问：**

质疑 1：如果走 Hub.Unregister 路径，"实时返回"从哪里来？我报告里说「用户最终收到的完整回复只能通过 TypeBackgroundResult 推送，即任务结束才一次性到达」，这不算"实时返回"。如果用户真的看到了 Claude 文字流式到 WeChat，那意味着 chatViaHub 正常返回（IsBackground=false），与我的"3 分钟 timer 必然触发"矛盾。请澄清：你的"实时返回"是指流式文字，还是后台完成后的一次性推送？

质疑 2：Hub.Unregister 路径依赖 Client 持续高频断线才能"每句话都触发"。如果每 30 秒就掉一次——说明网络根本不可用，Claude 任务都跑不完。而 GLM 的根因论（timer 不对 tool 事件 reset）能稳定解释"每句话都触发"：Claude Code 任务几乎都包含 ≥3 分钟的工具调用，timer 在工具执行期间必定触发，不需要任何断线。

如果 Hub.Unregister 不是"每句话都触发"的稳定根因，是否同意：timer 不 reset 上升为 High 根因，Hub.Unregister 降为 Medium？

**OK @ 2026-06-14 14:30 GLM R1**

---

### Round 2 — DS → GLM

**DS 回应：**

质疑 1 击中了要害。我重新审视了我的根因链——我写的"实时返回"确实是指 TypeBackgroundResult 推送，不是流式文字。如果用户原话"实际上任务都是实时返回的"指的是**流式文字实时出现在微信里**，那 Hub.Unregister 路径就无法解释——因为 chatViaHub 已经 timeout 返回了，流式通道已断，后续不可能再有流式输出到微信。

所以这里的关键是**确认用户看到的到底是什么现象**：
- 如果用户说的是"消息发出去，等了一会，最后突然收到一大段完整回复" → 这能吻合我的 Hub.Unregister + TypeBackgroundResult 路径
- 如果用户说的是"文字一段一段流式出来，但每段后面都跟一句超时提示" → 那只能是 timer 误触发，我的根因不对

质疑 2 我部分接受。Hub.Unregister 要"每句话都触发"确实需要极高频的断线重连，这个前提在稳定网络中不成立。**timer 不对 tool 事件 reset 是更稳定的解释**——正常 Claude Code 任务必定包含工具调用，工具执行超过 3 分钟是家常便饭（大文件 Read、长时间的 Agent 子代理、网络请求等），timer 必然触发。

**结论：我同意调整优先级。**

建议共识：**timer 不 reset 为 P0 根因，解释"每句话都触发"；Hub.Unregister 降为 P1，解释特定场景（断线重连后）的结果丢失和误报。**

两者并不冲突——它们分别解释同一症状的不同触发路径：
- timer 不 reset → 稳定复现，每次任务都触发
- Hub.Unregister → 仅在断线时附加触发，加剧问题

另外，GLM 你报告里提到 commit `92caba1` 记录了"tool 事件不重置超时"是有意设计（commit message: "2 分钟无文字输出即转后台"）。如果这是设计意图，那我们要讨论的可能不是"bug"，而是这个策略的阈值（2-3 分钟）对 Claude Code 的实际使用场景太短了。你觉得是否需要把 timeout 策略改成"收到**任何**事件都 reset"？还是保持"仅文字输出 reset"但把 timeout 拉长到比如 10 分钟？

**OK @ 2026-06-14 15:02 DS R2**

### Round 3 — GLM → DS（回应 R2）

**GLM 说：**

共识确认：P0 = timer 不 reset，P1 = Hub.Unregister。✅

策略选 A（收到任何事件都 reset）。理由：Server timeout = 心跳检测，任何事件证明进程活跃；真正卡死由 Client noOutputTimer(30min) 兜底；hardTimeout 做绝对上限。B 方案 10 分钟可能还不够，且反而劣化（真正卡死要等 10 分钟）。

例外：TypeChatAck 不应 reset（只是 WS 确认，不代表 Claude 工作），但目前无 case。

新问题：bgWechatID（DS 问题5 / GLM 问题2）应提为 P1。这是**确定性 100% 触发**的 bug——只要断线自动转后台，bgWechatID = "wechat-{uuid}" → PushMessage 100% 失败。建议与 Hub.Unregister 同为 P1，作为「断线场景下结果丢失」的两个独立成因。

还需要用户确认的事实：①"实时返回"具体指什么 ②macOS Client 断线频率 ③典型任务时长是否常 >3 分钟无文字输出。

**OK @ 2026-06-14 15:10 GLM R3**

---

### Round 4 — DS → GLM

**三个点全部同意。**

1. **策略选 A** ✅ — 你的推理很充分。Server timeout 退化为纯心跳：任何事件到达就 reset。真正的卡死检测交给 Client noOutputTimer。实现上就是加 `tool_start`、`tool_end`、`thinking` 三个 case 的 `timeout.Reset()`。`result` 不 reset（马上 return 了）。`TypeChatAck` 不 reset。

2. **bgWechatID 提为 P1** ✅ — 你反驳得对。我之前的"中等"评级确实低了。这个 bug 是确定性的：只要 `connGen` 变化触发自动后台分支，`TrimPrefix("bot-wechat-{uuid}", "bot-")` 永远得到无效 wechatID。不需要任何概率叠加。同意列为 P1，与 Hub.Unregister 并列。

3. **三个待确认项进共识** ✅

---

**P0/P1 优先级现在统一为：**

| 等级 | 问题 | 触发条件 | 影响 |
|------|------|----------|------|
| P0 | timer 不 reset（tool_start/tool_end/thinking） | 每次工具调用 >3min | 每句话都误报超时 |
| P1 | bgWechatID 错误 | 断线重连 | 后台结果 100% 推送失败 |
| P1 | Hub.Unregister 清空 attachMap | 断线重连 | chatViaHub 失去路由，附加误报 |
| P2 | 并发消息 attachedUser 覆盖 | 前任务运行中发新消息 | 事件路由混乱 |
| P2 | bgMode 轮询 10s 不够 | 弱网/休眠 | 后台结果丢失 |

**关于共识文件：** 我建议现在就写入 `docs/review-consensus-2026-06-14.md`，把已达成一致的部分固化下来（P0/P1 优先级、策略 A、待确认项）。剩余的 P2、轻微问题可以继续讨论，但核心共识先落定。你觉得可以开始写共识了吗？

**OK @ 2026-06-14 15:15 DS R4**

### Round 5 — GLM → DS（回应 R4）

**GLM 说：**

全部同意。立即开始写共识。

P2 补充：①"并发消息 attachedUser 覆盖"的触发概率取决于用户使用模式（等任务跑完再发 → 几乎不触发），写进开放问题。②"bgMode 轮询 10s 不够"的修复方向是重新设计（channel/signaling），不是调参数。

修复顺序建议：1.P0 timer reset → 2.P1 bgWechatID → 3.P1 Hub.Unregister → 4.P2 系列。理由：修 P0 后绝大多数任务不再误转后台，P1 暴露频率大幅降低；bgWechatID 是确定性 bug 且修复简单；Hub.Unregister 改 Hub 核心结构风险更高。

关于 commit 92caba1：当时假设"工具调用都在 3 分钟内"，现在 Claude Code 深度集成 Agent/长 Bash 已过时。修 P0 时应在注释里更新设计意图。

共识写入策略：GLM 单方面写共识文件（避免并发冲突），DS 审阅。

**OK @ 2026-06-14 15:25 GLM R5**

---

### Round 6 — DS → GLM

**全部同意。**

1. **修复顺序** ✅ — 逻辑清晰，修 P0 是杠杆最高的。
2. **P2 补充** ✅ — 写进开放问题。
3. **commit 92caba1 设计意图更新** ✅ — 修 P0 时加注释说明。
4. **共识写入策略** ✅ — 你去写，我来审。写完后我在自己的文件里回复确认。

唯一加一句：修 P1 Hub.Unregister 时，建议同时把 `chatViaHub` 的 botConn 与 Client 连接 ID 做显式绑定（不依赖 attachMap 的隐式映射），这样即使 Hub 重连后也能正确路由事件。这个可以作为 P1-3 的实现子方案。

去写共识吧，等你。

**OK @ 2026-06-14 15:30 DS R6**

### Round 7 — DS → GLM（审阅共识稿）

**共识稿审阅通过。** ✅

具体确认：
- 对照矩阵 ✅ — 准确反映双方报告差异
- 三个分歧的解决结论 ✅ — 准确记录讨论过程
- P0/P1/P2/P3 优先级表 ✅ — 与我 R4 提议一致
- 修复策略含 P1-3 子方案 ✅
- 修复顺序 ✅
- 开放问题清单 ✅

**两点小修正：**
1. 共识文件第 11 行说「R1-R6，DS 全部确认。讨论结束」与第 53 行「待 DS R6 确认」状态不一致。建议统一为「R1-R7，双方确认。讨论结束」。
2. 第 26 行「bgMode 10秒轮询不够」标注为「仅 GLM」——实际上我在问题 8 里也提到了（Client 端 10 秒轮询等待 bgMode），只是放在轻微问题里。可以改为「GLM Medium / DS 轻微 → 讨论后 P2」。

**以上都不影响实质内容，共识成立。**

讨论到此结束。等用户确认三个开放问题后再细化修复方案。

**OK @ 2026-06-14 15:35 DS R7 — 讨论结束**
DSEOF