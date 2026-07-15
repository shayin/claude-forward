# 议题: 服务端重启后，client 重连时疯狂往微信推消息（断线事件回放 + 顺序错乱）

**日期**: 2026-07-15
**参与方**: Pilot (glm) × Copilot (deepseek)
**讨论方式**: 单窗口主驾-副驾模式（Cross 模式 Direction 1）

## 状态
已达成 ✅

---

## Round 1 - Pilot

### 现象（用户报告）

服务端（云服务器）重启后，client 重连时**疯狂往微信推消息**。表现为：像是断线期间堆积的事件被**一次性回放/重发**到微信，且消息**顺序错乱**。

### 架构背景

- 链路：手机 Web UI `<--WS-->` 云服务器 Server `<--WS-->` 本地 Client
- 微信消息走 Bot 通道：连接 ID 以 `bot-` 前缀，由 server 的 `wechat.go` `chatViaHub` 创建**一次性虚拟 bot 连接**（`bot-wechat-<uuid>`），任务结束即 detach + cleanup。
- Bot 通道与 Web UI 通道在 client 端走不同分支（`isBot := strings.HasPrefix(userID, "bot-")`）。

### 代码现状梳理（仅描述事实，不下结论）

#### 1. `c.send` channel 的生命周期（核心嫌疑）

- 在 `NewClient` 里**创建一次**：`make(chan *protocol.Message, 256)`（`connection.go:62`）。
- 全仓 grep `c.send` / `close.*c.send` 确认：**没有任何地方 `close(c.send)`，也没有重新 `make`**。整个 Client 生命周期共用同一个 buffered channel。
- `writePump`（`connection.go:258-319`）每次 `Connect()` 启动一个新实例（`connection.go:115`），消费的是同一个 `c.send`。
- `writePump` 的退出路径：`conn.WriteMessage` 失败（`:301-303`）、`conn == nil || connClosed == 1`（`:284`）、`ctx.Done`（`:272`）、channel close（`:276`）。

**事实链**：连接断开 → `writePump` 的 `WriteMessage` 失败 → `writePump` return（消费者退出）。此时：
- 已经在 `c.send` 里的消息；
- 以及 `writePump` 退出后生产者继续写入的消息；
- **全部留在 channel 里无人消费**，直到重连后新 `writePump` 启动把它们 flush 出去。

#### 2. 断线期间的生产者持续写入

`c.send` 的写入点非常多（grep 得到 ~20 处），其中与微信相关的关键写入者：

- `handleChatInput` goroutine（`:593`）**独立于 WebSocket 连接**，任务在断线期间继续运行。
- 后台模式自动触发：`connection.go:758` `if !bgActive && atomic.LoadInt64(&c.connGen) != startGen` → 设 `bgMode=true`、`bgWechatID = c.currentWechatID`。
- 后台模式下 bot 通道**跳过逐条事件发送**（`:722` `if uid != "" && !bgActive`），但任务结束时仍会构造 `TypeBackgroundResult` 写入 `c.send`（`:904-913`）：
  ```go
  select {
  case c.send <- resultMsg:            // ← 断线时 writePump 已退出，写入成功（缓冲未满），消息堆积
  case <-time.After(30 * time.Second):
      c.savePendingResult(...)         // ← 只有阻塞 30s 才落盘
  }
  ```

#### 3. `BackgroundResult` 的路由路径（绕过 attachMap）

- `handler.go:207` `case TypeBackgroundResult: h.handleBackgroundResult(conn, msg)`。
- `handleBackgroundResult`（`handler.go:397-427`）**直接** `h.wechatMgr.PushMessage(payload.WechatID, text)`。
- **不检查来源连接是否还有效，不依赖 `attachMap`，不依赖任何 bot 连接是否还在**。只要 `WechatID` 在白名单配置里，就推送。

对比：普通的 `TypeChatMessage` / `TypeChatReady` 走 `handleChatClientToUser`（`handler.go:466`），它**依赖 `attachMap`** 反查 user：`for userID, clientID := range h.hub.attachMap { if clientID == conn.ID ... }`。服务端重启后 `attachMap` 为空、旧 `bot-wechat-<uuid>` 连接也已失效，这类消息**理论上找不到 targetUser 而被丢弃**。

#### 4. `pending_results` 磁盘机制

- `savePendingResult`（`:1044`）：写入**单文件** `~/.claude-forward/pending_results/{clientID}.json`，**覆盖式**（多个任务只留最后一条）。
- `resendPendingResults`（`:1063`）：在 `Connect()` 成功后立即调用（`:120`），读文件 → 发 `TypeBackgroundResult` → 删文件。
- 与 `c.send` 堆积的关系：`BackgroundResult` 写 `c.send` 成功则**不落盘**；只有 `c.send` 写入阻塞满 30s 才落盘。

#### 5. `connGen` 连接代数

- 声明 `:40`，`Connect()` 里递增 `:77`（`atomic.AddInt64(&c.connGen, 1)`）。
- `handleChatInput` 开始时记录 `startGen := atomic.LoadInt64(&c.connGen)`（`:675`），事件循环里 `:758` 检测 `connGen != startGen` 判断是否断线重连。
- **断线重连必然触发**（因为每次 `Connect` 都递增）。

#### 6. `sessionEvents` 回放（仅 Web UI，Bot 不走）

- `TypeAttach` 处理（`:356-399`）：`bot-` 前缀**跳过回放**，直接 `setUser(msg.From)`；只有 Web UI 才回放 `sessionEvents` 快照→增量→setUser 三步。
- `sessionEvents` 持久化到磁盘（`loadSessionEvents`/`saveSessionEvents`），`sentUpTo` 去重。
- **Bot 通道不写 `sessionEvents`**（`:601` 注释明确 "Bot API 使用独立 session，不存 sessionEvents"），所以微信侧理论上没有 sessionEvents 回放路径。

#### 7. `currentWechatID` 是 client 级单值

- 字段 `:54` `currentWechatID string`，**单值**，非 per-connection。
- `TypeChatInput` 时设置（`:452-457` `c.currentWechatID = payload.WechatID`）。
- 后台模式自动触发时用它当 `bgWechatID`（`:761` `c.bgWechatID = c.currentWechatID`）。

#### 8. 服务端重启后的状态

- `Hub` 全新：`clients` / `users` / `attachMap` **全空**。
- 旧的 `bot-wechat-<uuid>` 连接 ID 全部失效。
- `WeChatManager.Start()` → 对已恢复登录的用户调用 `flushPushQueue(idx, user)`（`wechat.go:95,184`），投递离线 push 队列。
- `pollLoop` 重新启动，`GetUpdates` 可能批量拉回断线期间微信用户发的多条消息，每条 `go m.handleMessage(...)`（`wechat.go:259`）**并发处理**。

### 开放问题（不下结论，留给后续轮次）

1. **`c.send` 堆积回放**：断线期间写入 `c.send` 的消息（尤其 `BackgroundResult`），重连后被新 `writePump` 全部 flush，是否就是"疯狂推送"的主路径？这些消息的 `WechatID` 字段在 server 侧被直接 `PushMessage`，无任何去重或时效校验。
2. **多个 `BackgroundResult` 的来源**：engine 有 `IsRunning()` 串行锁（`:623`），单时刻只跑一个任务。那"多条" `BackgroundResult` 是怎么在断线期间堆积出来的？是断线期间任务依次结束（task1 后台结束→task2 因新消息启动→又结束），还是别的路径？
3. **消息顺序错乱**：`c.send` 是单 channel FIFO，单生产者保序。但断线期间若多个 goroutine 并发写（多个 `handleChatInput` 实例 / `sendChatText` 等）会交错。或者顺序错乱来自 **server 侧** `pollLoop` 批量拉取 + 并发 `handleMessage` + 各自独立 `chatViaHub`？
4. **`currentWechatID` 串扰**：client 级单值，断线期间若有多个微信用户的请求交错，`bgWechatID` 是否会推给错误的微信用户？
5. **磁盘 vs 内存双发**：`resendPendingResults`（磁盘，1 条）与 `c.send` 堆积（内存，N 条）两条路径在重连后**同时** flush，是否可能对同一任务结果双发？
6. **`flushPushQueue` 参与**：server 重启后投递离线 push 队列，这个队列在断线期间由谁积累？是否参与"疯狂推送"？
7. **in-flight 消息丢失**：连接断开瞬间，`writePump` 已从 `c.send` 取出但 `WriteMessage` 失败的消息会丢失（已出 channel）。这与"堆积回放"是两个不同子问题，需厘清边界。
8. **`To` 字段时效**：堆积消息的 `To` 是旧 `bot-wechat-<uuid>`。普通 ChatMessage 因 `attachMap` 重启清空而被丢弃，但 `BackgroundResult` 不看 `To`、只看 payload 里的 `WechatID`——这个不对称是否正是"只有 BackgroundResult 类消息被疯狂重发、而 ChatMessage 类被静默丢弃"的原因？

OK @ 2026-07-15 18:48 Pilot R1


---

## Round 2 - Copilot

### 总体判断

Pilot R1 的代码梳理非常详尽，事实陈述几乎没有错误。我同意**核心嫌疑在 `c.send` 堆积回放 + `handleBackgroundResult` 绕过 attachMap** 这条主链。但以下 3 点补充了 Pilot 没有展开或没有触及的关键角度。

---

### 1. `c.send` 的 30 秒超时写入给了虚假安全感——大部分 BackgroundResult 根本不会落盘

Pilot R1 第 2 点指出 `savePendingResult` 只在 `c.send` 写入阻塞满 30 秒后才触发。这个观察是对的，但没有强调它的实际后果。

来看 `connection.go:912-925`：

```go
select {
case c.send <- resultMsg:            // ← 缓冲未满时立即成功，不落盘
case <-time.After(30 * time.Second):
    c.savePendingResult(...)         // ← 只有缓冲满了才落盘
}
```

`c.send` 容量 256。在典型的断线窗口（几十秒到几分钟）内，一个 `handleChatInput` goroutine 的 event loop 最多生产 1 条 `BackgroundResult` + 若干 `ChatAck` / `ChatReady` / `ChatError`。即使考虑多个并发任务，**总量几乎不可能达到 256**。这意味着 `savePendingResult` 路径几乎从不触发——消息全堆在 channel 里。

同时 Pilot 提到 `pending_results` 文件是**覆盖式单文件** (`connection.go:1044`)，这是个正确的发现。但合起来看：磁盘机制既不可靠（单文件覆盖丢失多条），又不太被触发（channel 很少满），整体形同虚设。

**这意味着**：断线期间产生的 BackgroundResult **既不在磁盘上（被 channel 缓冲吞了），也不被消费（writePump 已退出）**，全部悬在内存里。重连后 writePump 统一 drain，形成"疯狂推送"。这就是主路径。

**修复方向**：断线时（`Disconnect()` 或 `Run()` 中 `<-c.ctx.Done()` 之后、启动新 `Connect()` 之前），drain `c.send` 中所有残留消息，将其中 `TypeBackgroundResult` 类型的 persist 到磁盘（用 taskID 做 key，避免覆盖），其余丢弃。这样重连后只有磁盘路径（`resendPendingResults`）可控地重放，不会有 channel 堆积的 N 条消息一次性轰炸。

---

### 2. "顺序错乱"有两个独立来源，Pilot 的分析把它们混在一起了

Pilot 开放问题 #3 提到两种可能：`c.send` 多 goroutine 并发写入交错，或 server 侧 `pollLoop` 并发消息处理。但这两个问题有本质区别：

**来源 A — `c.send` 堆积 flush 与重连后新消息的时序交叠：**

`c.send` 是 FIFO channel，单生产者保序没错。但"序"的参照系是**微信用户发消息的时间顺序**，不是 channel 的入队顺序。断线期间的 BackgroundResult 入队时间在 T1~T2（断线中），重连后新消息产生的 BackgroundResult 入队在 T3（重连后），但它们都从同一个 `c.send` 被 `writePump` 顺序消费。如果把 channel 想象成一根管道，断线期间倒进去的老水和重连后倒进去的新水混在一起流出——**管道内部 FIFO 并不能保证外部时间线的语义保序**。

更具体地说：微信用户发了消息 A → 任务 A 在断线期间后台完成 → BackgroundResult(A) 入 `c.send` → 重连 → 用户发消息 B → 任务 B 完成 → BackgroundResult(B) 入 `c.send` → 但 BackgroundResult(A) 排在前面先被发送 → 用户先收到 A 的结果再收到 B 的结果。这里面顺序是"对"的（A 确实比 B 先入队），但用户体验是"我刚刚才问 B，为什么先回了 A？"——这是**时间域错乱**，不是 channel 乱序。

**来源 B — server 侧 `pollLoop` 并发 `handleMessage` (`wechat.go:259`)：**

```go
for _, msg := range msgs {
    go m.handleMessage(idx, user, msg)  // 并发！
}
```

服务端重启后，`GetUpdates` 可能批量拉回断线期间微信用户发的 N 条消息。每条消息 `go handleMessage` 并发处理，各自 `chatViaHub` 创建独立的 `bot-wechat-<uuid>` 连接。Client 端虽有 `IsRunning()` 串行锁（`connection.go:623`），但**哪个 goroutine 先抢到锁完全是随机的**，不保证与微信消息的原始发送顺序一致。

这两个来源叠加：来源 B 导致任务启动顺序不可控，来源 A 导致结果推送顺序与用户理解的时间线不一致。

**修复来源 A**（drain channel）可以在很大程度上消除"疯狂推送+错乱"的 UX 问题。修复来源 B 需要将 `pollLoop` 改为串行处理（或至少按 `msg.Seq` / 时间戳排队），这是独立议题。

---

### 3. Pilot 漏掉了一个关键事实：`writePump` 在取消息和写消息之间有竞态窗口

Pilot R1 第 1 点描述了 `writePump` 退出后消息堆积在 channel 里，但没提到 `writePump` 退出**瞬间**的 in-flight 竞态。看 `connection.go:275-303`：

```go
case msg, ok := <-c.send:        // ← 消息已从 channel 取出，不可恢复
    // ...
    c.mu.Lock()
    conn := c.conn               // ← 快照连接
    c.mu.Unlock()
    // ...
    if err := conn.WriteMessage(...); err != nil {
        return                    // ← 写失败，消息丢失！
    }
```

时序：`<-c.send` 取出消息 → 获取 conn 快照 → 写 WebSocket → 如果此时连接已断（`Disconnect()` 被另一个 goroutine 调用），`WriteMessage` 失败 → `writePump` return。**这条消息已经从 channel 消费了，不会堆积，但也永远不会被送达。** 这是"in-flight 丢失"（Pilot 开放问题 #7）。

这个问题与"堆积回放"是一体两面：堆积的消息会被过量重放，in-flight 的消息会无声丢失。两者都源自同一个架构缺陷——**`c.send` 没有连接生命周期感知**。

**修复**：在 drain channel 的同时，不需要特别处理 in-flight 丢失（因为它本质上是不可恢复的——消息已经离开 channel，无法回退）。但可以在消息体中加一个 `connGen` 字段（client 端生产时打上当前连接代数的戳），server 端在 `handleBackgroundResult` 中比对当前 client 的连接代数——如果消息的 `connGen` 不等于当前代数，说明是旧连接周期的残留，直接丢弃。这样即使 drain 不彻底（有漏网之鱼），server 侧也能兜底拦截。

---

### 小结

| 问题 | 根因 | 修法 |
|------|------|------|
| 疯狂推送 | `c.send` 缓冲在断线期间堆积 BackgroundResult，重连后 flush | 断线时 drain channel + persist BackgroundResult 到磁盘 |
| 顺序错乱 | (A) 新老消息在 channel 里按入队时间交错 + (B) server 并发 pollLoop | 修 A 即可大幅改善；B 是独立问题 |
| 重复推送 | 磁盘 `pending_results` 单文件覆盖 + channel 堆积 两条路径同时存在 | drain 后只用磁盘路径，消灭双路径 |
| 跨连接代数串扰 | `handleBackgroundResult` 无条件信任 payload.WechatID | server 侧加 connGen/timestamp 校验 |

**倾向的方案**：优先做 client 端 drain-on-disconnect，这是最小改动、最大收益的切入点。server 侧加时间戳校验作为兜底。两项加起来应该能消除 90%+ 的"疯狂推送"现象。

OK @ 2026-07-15 18:55 Copilot R2

---

## Round 3 - Pilot

### 总体：主链一致，接受副驾的 3 个补充

副驾 R2 的主链判断（`c.send` 堆积 + `BackgroundResult` 绕过 attachMap）与我的 R1 一致。它补充的 3 点我**全部接受**：

- 30s 超时落盘形同虚设 ✓
- `writePump` in-flight 竞态（取出消息→写失败→丢失，与堆积是"一体两面"）✓
- 顺序错乱两个独立来源（A: channel 入队时间交错 / B: server `pollLoop` 并发）✓

但我要**强化**其中一点的定性，并对它的修复方案提出**修正与部分反对**。

### 1. 强化定性：30s 超时不是"形同虚设"，而是"不可达代码"

副驾说 `savePendingResult` "几乎从不触发"。我更进一步——在本 bug 场景下它是**逻辑上确定性不可达的**：

`c.send` 是 buffered channel（`cap=256`）。`case c.send <- resultMsg`（`:913`）在 buffer 未满时**立即成功返回、不阻塞**，Go 的 `select` 会选中这个就绪 case。只有当 `c.send` 已堆积 **256 条未消费**消息时，写入才阻塞，30s 后才走落盘分支（`:915-924`）。

而断线期间能堆积多少条 BackgroundResult？engine 有 `IsRunning()` 串行锁（`:623`），**同一时刻只有一个任务在跑**；单个 `handleChatInput` goroutine 后台模式结束时**只产 1 条** BackgroundResult（`:904`）。1 ≪ 256，落盘分支**在该场景下永远走不到**。

这个定性的意义在于揭示**作者的本意与实现的错位**：写这段 30s 超时落盘，本意是"连接断了、写不进 `c.send` 就落盘兜底"。但 buffered channel 的非阻塞写入让"写不进"这个条件**永不满足**——所以兜底从一开始就没生效过。这不是调参问题（不是"30s 太短"或"buffer 太大"），是被 buffered channel 语义骗了的**逻辑错误**。

### 2. 对副驾 drain-on-disconnect 方案的修正：治标，且有时机难题

副驾建议"断线时 drain `c.send` + persist 其中的 BackgroundResult"。我认为这是**治标**，且有一个副驾未触及的**时机难题**：

**时机难题**：drain 只有在"断线且所有在途任务都已结束"之后才彻底有效。但 `handleChatInput` goroutine **独立于连接生命周期**——断线瞬间任务往往还在跑。若 drain 选在 `Disconnect()` / `<-c.ctx.Done()`（此时任务未结束），drain 之后任务才结束、才产生 BackgroundResult 写入 `c.send`，**这条照样堆积**。若等任务结束再 drain，又无法统一收敛（多个任务异步结束、无统一汇合点）。drain 治不了"drain 之后到达"的消息。

**治本方案（我的倾向）——从源头不让 BackgroundResult 进 `c.send`**：

后台模式（`bgActive=true`）的语义本就是"无人在线接收、完成后异步推送"。当 `bgActive` 是由 **connGen 变化触发**（`:758`，即真正的断线）时，`writePump` 大概率已退出，`c.send` 此时不可靠。所以 `:912-925` 应改为分支处理：

```go
if bgActive && (因 connGen 断线触发) {
    // 断线后台：直接落盘，绕过 c.send（c.send 此时不可靠）
    c.savePendingResult(payload)   // 改成 append/多文件按 taskID
} else {
    // 在线后台（server 主动设的超时后台，:511 TypeBackgroundMode）：连接还在，走 c.send
    c.send <- resultMsg
}
```

这样断线期间的 BackgroundResult **根本不进 `c.send`**，重连后只走 `resendPendingResults`（磁盘、可控、单条或按 taskID 多条）这一条路径。副驾想要的"消灭双路径"自然达成，且绕开了时机难题——因为根本不需要 drain。

**关键实现点**：要区分两种 `bgActive` 来源——
- server 主动下发的超时后台（`TypeBackgroundMode`，`:511`，此时 WebSocket 可能还活着）→ 走 `c.send`
- client 自检的断线后台（connGen 变化，`:758`）→ 走落盘

副驾提的"savePendingResult 改多文件/按 taskID 避免覆盖"，我**同意**，这是源头落盘方案的必要配套（否则多条断线结果互相覆盖）。

### 3. 补充：顺序错乱"来源 A"即使源头治理也解决不了，属于产品决策

副驾认为修来源 A（drain）能"在很大程度上消除"错乱 UX。我**保留意见**：

即使 BackgroundResult 全走磁盘路径、`c.send` 零堆积，重连后 `resendPendingResults` 会**先**把断线期间的旧任务结果（A）推出去，**然后**才是重连后新任务的结果（B）。用户看到的是"我刚问 B，为什么先收到 A 的结果"——这个语义错乱**与 drain / 堆积无关**，本质是"旧结果是否还要推送、如何呈现"的问题。

要彻底解决需要**产品决策**，三选一：

- **选项 1（推荐）**：断线旧结果照推，但加显式前缀标记，如 `⏮ [断线前任务结果] ...`，让用户理解为何顺序"倒"了。
- **选项 2**：断线旧结果直接丢弃，重连后只给一条提示"断线前有任务完成，结果已丢弃，请重新提问"。
- **选项 3**：按时间戳排序——但 A 产生于断线中、B 产生于重连后，时间戳天然 A<B，排序后仍 A 先，无效。

我倾向**选项 1**（加标记），因为丢弃用户苦等的后台结果体验更差。但这超出"修 bug"范畴、属产品取舍，**需用户拍板**，不宜由我们单方面定。

### 倾向总结

| 维度 | 我的倾向 | 与副驾 R2 的差异 |
|------|---------|-----------------|
| 根因定性 | 30s 超时是**确定性不可达代码**（buffered channel 非阻塞写入） | 副驾"形同虚设"偏概率描述；我强化为逻辑错误 |
| 主修复 | 断线触发的后台模式：BackgroundResult **源头直接落盘**，不进 `c.send` | 副驾主张 drain-on-disconnect（我认为治标 + 时机难题） |
| 磁盘机制 | `savePendingResult` 改 append / 按 taskID 多文件 | **一致** |
| 顺序来源 A | 源头治理也解决不了，需产品决策（推荐加 `⏮` 标记） | 副驾认为 drain 能改善；我保留意见 |
| server 兜底 | `handleBackgroundResult` 按 taskID 幂等去重 + 可选时间戳 | 接近一致；副驾提的 connGen 校验需改协议，taskID 已存在、更轻量 |

**核心分歧**：drain-on-disconnect（副驾）vs 源头落盘（我）。我认为源头治理更干净、绕开时机难题；drain 作为"防御性补充"可以保留，但不能作为主修复——因为它管不住"drain 之后到达"的消息。

OK @ 2026-07-15 18:56 Pilot R3

---

## Round 4 - Copilot

### 总体

R3 对 drain 方案的"时机难题"批评**我接受**。drain 在 `Disconnect()` 或 `Connect()` 前执行，确实管不住之后才完成的在途任务产生的消息。这不是执行位置可以调的——只要在途任务的生命周期没有统一的汇合点，drain 就是不可靠的。

但 R3 提出的替代方案（源头落盘 + 按 bgMode 触发来源分流）**存在一个更本质的问题**，以及两个次要但值得指出的过度陈述。

---

### 1. "按触发来源分流"的核心缺陷：入口状态代理出口状态

Pilot 的核心思路是区分两种 bgActive 来源（server TypeBackgroundMode vs client connGen 自检），断线触发的直接落盘、在线超时的走 `c.send`。

**问题：bgActive 触发时刻的"连接状态" ≠ BackgroundResult 产生时刻的"连接状态"。**

构造一个边界场景：

```
T1: Server chatViaHub 3分钟超时 → 发送 TypeBackgroundMode 给 Client
    → Client handleMessage 设 bgMode=true（来源=server，Pilot 说"走 c.send"）
T2: Server 崩溃（在发送 bgMode 之后、tgMode 被处理之后）
T3: Client 检测断线 → readPump 退出 → writePump 退出
T4: 任务在后台完成 → BackgroundResult 产生
    → 按 Pilot 的方案：来源=server → 走 c.send ← 但 c.send 的消费者已死
    → BackgroundResult 写入 buffered channel，无人消费，堆积。
```

同理反过来也可以构造：

```
T1: connGen 自检触发 bgMode（来源=connGen，Pilot 说"走落盘"）
T2: Client 重连成功，新 writePump 启动
T3: 任务完成 → BackgroundResult 产生 → 走落盘 → pending_results.json
T4: 但此时 c.send 其实是可用的！下次 Connect() 时 resendPendingResults 才发
    → 不必要的磁盘 IO + 延迟
```

**根因**：Pilot 试图用"当初为什么进入后台模式"来回答"现在 c.send 是否可用"。但这两个问题的答案在时间上可以发散。入口状态和出口状态之间有任意的延迟（任务可能跑几秒也可能跑几十分钟），期间连接状态可以任意变化。

**比"来源"更可靠的判定指标是 `startGen`**。每个 `handleChatInput` 都在 `:675` 记录了 `startConnGen`。在 BackgroundResult 产生时（`:902-925`），直接比对：

```go
if atomic.LoadInt64(&c.connGen) != startGen {
    // 任务生命周期内发生过断线重连 → c.send 不可靠 → 落盘
    c.savePendingResult(payload)
} else {
    // 同一连接代数 → c.send 的 writePump 始终是同一个 → 安全使用
    c.send <- resultMsg
}
```

这不需要区分 bgMode 的"来源"，不需要新增状态字段，判定依据直接就是"c.send 还在不在原来的写循环里"。这既保留了 Pilot"源头不入 c.send"的干净性，又修复了来源标记的时效问题。

Pilot 可能在意的一个点：`startGen` 判断是否会误判"在线超时→同一连接代数→走 c.send→但此时 conn 恰好断了"？不会，因为在线超时时 WebSocket 还活着（否则 TypeBackgroundMode 消息根本发不到 Client），`writePump` 还在消费，`c.send` 可用。后续如果又断了，`connGen` 会递增，下一次检测就会走落盘。这正是想要的行为——**在 BackgroundResult 产生的那一刻做决策，而不是在 bgMode 被设置的那一刻**。

---

### 2. "不可达代码"定性：在当前场景下正确，但表述过于绝对

Pilot 说 `savePendingResult` 的 30s 超时路径在当前 bug 场景下"确定性不可达"——因为 `IsRunning()` 串行锁保证同时最多 1 个任务，单任务最多产 1 条 BackgroundResult，`1 ≪ 256`。

**在当前单 bot 任务场景下这是对的**，我收回 R2 中"256 条不可能达到"的表述——不够精确。Pilot 的 `1 ≪ 256` 数字上更准确。

但有一个边界：`c.send` 是 client 级别的单例（`NewClient` 只 `make` 一次），所有通道（Web UI + 多个 bot-wechat-* 连接）**共享同一个 256 buffer**。虽然 Bot 通道在 connGen 自检触发 bgActive 后不再逐条写事件（`:722`），但 Web UI 通道**不经过 bgActive 门控**（`:772-788` 只检查 `uid != "" && shouldSend`）。如果断线时恰好 Web UI 连接活跃，stream_delta 等事件会持续写入 `c.send`，每个事件占一个 slot。

Web UI 本身走 `sessionEvents` 机制不必担心堆积，但它的写入**挤占 `c.send` 的 buffer 容量**。如果 Web UI 的流式事件写了 200+ 条进 `c.send`，剩给 BackgroundResult 的 slot 可能就不够了——此时 30s 超时路径变成可达。

这个场景发生的概率低，但它说明"确定性不可达"这个表述依赖于"断线时没有 Web UI 连接活跃"这个隐性前提。定性修正为"**在当前典型单 bot 场景下不可达，但不能排除多通道并发的边界情况**"更准确。

---

### 3. 顺序来源 A：不需要产品决策，有纯技术方案

Pilot 说来源 A（旧结果在新结果之前到达）"即使源头治理也解决不了"，并给出三个产品选项。

**我不同意。`resendPendingResults` 在 `Connect()` 后立即推送（`:120`）不是唯一设计**。

一个纯技术的替代：**延迟重放**。

```
当前行为：
Connect() → resendPendingResults() → 旧结果 A 立即推送 → 新消息 B 到达 → 新结果 B 推送
用户看到：A, B ← "我刚问 B 为什么先收到 A？"

改为：
Connect() → resendPendingResults 只标记 pending 但不推送
         → 新消息 B 到达 → 新结果 B 推送成功
         → 在 B 的结果处理完成后，触发 flushPendingResults
         → 旧结果 A 以 "⏮ [之前的后台结果] ..." 格式推送
用户看到：B, ⏮A ← 顺序符合直觉
```

实现不需要新增协议字段，只是 client 端的调度优化：

1. `resendPendingResults` 改为 `markPendingResults`——只把旧结果存入内存待推送队列（不做 `c.send <-`）。
2. 在 BackgroundResult 正常送达后（`c.send <- resultMsg` 成功），flush 待推送队列。
3. flush 时追加 `⏮` 前缀。
4. 加上超时兜底：如果重连后 N 分钟都没有新 BackgroundResult（用户可能不再发消息），也把旧结果推出去。

这个方案**既不丢失旧结果，又保证语义顺序正确**，不需要用户参与决策。Pilot 的三个选项里选项 1（加标记）是最优的，但没必要让用户拍板——标记 + 延迟推送是纯粹的技术实现细节。

---

### 修正后的方案倾向

| 维度 | 修正意见 |
|------|---------|
| 主修复 | 同意"源头不入 c.send"，但判定条件用 `startGen != currentConnGen` 而非 bgMode 触发来源 |
| 磁盘机制 | 同意 `savePendingResult` 改为按 taskID 分文件 / append，避免覆盖 |
| drain | 降级为防御性补充（在 `Connect()` 前清一次 channel），不作为主修复 |
| 顺序来源 A | 延迟重放（技术方案），不等产品决策 |
| server 兜底 | 同意 `handleBackgroundResult` 按 taskID 幂等去重 |

OK @ 2026-07-15 19:10 Copilot R4

---

## Round 5 - Pilot

### 接受 R4 的两个关键修正，部分接受第三个

R4 的 3 个反驳里，**第 1、2 点说服了我，我修正立场**；第 3 点部分接受。

#### 1. 接受：用 `startGen != currentConnGen` 替代"来源分流"（放弃 R3 主修复方案）

R4 第 1 点击中了我 R3 方案的真实缺陷：我用"bgMode 的**触发来源**"（入口状态）去推断"BackgroundResult **产生时** c.send 是否可用"（出口状态），但任务可能跑几秒到几十分钟，期间连接状态可以任意变化。R4 构造的边界场景（server 发 `TypeBackgroundMode` 后崩溃 → client 任务完成时按"来源=server"走 `c.send` → 但消费者 writePump 已死 → 仍堆积）确实让我的方案失效。

`startGen` 比对是在 **BackgroundResult 产生的那一刻**（`:902-925`）做决策，直接回答"c.send 还在不在原来那个 writePump 的写循环里"，不依赖入口状态、不需要新字段、不需要区分来源。比我说的"区分两种 bgActive 来源"更干净、更鲁棒。

**我放弃 R3 的"来源分流"，采用 R4 的 startGen 判定**作为主修复。

补充一点有利于落地的细节：`startGen` 已在 `:675` 记录，`:758` 已经在用它检测断线转后台。所以这个判定是**复用现有机制**，改动量极小——只需在 `:912` 的 select 之前加一个分支：

```go
if atomic.LoadInt64(&c.connGen) != startGen {
    c.savePendingResult(payload)   // 任务生命周期内断过线 → c.send 不可靠 → 落盘
} else {
    c.send <- resultMsg            // 同一代数 → writePump 是同一个 → 安全
}
```

#### 2. 接受：修正"不可达代码"的绝对定性

R4 第 2 点成立。我漏看了 `c.send` 是 **client 级单例**——Web UI 通道（`:772-788`）和所有 `bot-wechat-*` 连接**共享同一个 256 buffer**。Web UI 的流式事件不经 bgActive 门控，断线时若 Web UI 活跃，stream_delta 会持续写入挤占 buffer 容量。

修正定性为：30s 超时落盘路径在**典型单 bot 场景下不可达**（单任务产 1 条 BackgroundResult ≪ 256），但在**多通道并发边界**（断线时 Web UI 活跃挤占 buffer）下**可达**。这不改变主修复方向（startGen 判定让断线结果根本不进 `c.send`），但修正了我之前过于绝对的表述——R4 说得对。

#### 3. 部分接受：延迟重放——收回"需产品决策"，但指出实现边界

R4 第 3 点对一半：**加 `⏮` 标记确实是纯技术细节，不需要产品决策**。我 R3 把"加标记"和"丢弃/排序"混在一起说成"需用户拍板"，不准确，**收回**。

延迟重放作为**目标行为**我认可。但指出它比 R4 描述的"只是调度优化"有几个实现边界（不阻塞共识，列为实现注意点）：

- 新任务 B 若**也走后台/落盘**（startGen 不等），"B 的 c.send 成功后 flush pending"这个触发点会失效——需另设 flush 触发（如定时 flush，或 resend 时一并处理）。
- 用户重连后**不发新消息**：需超时兜底（R4 已提）。
- 多个旧结果 A1..An：全部 pending，flush 时按 taskID 顺序 + 统一 `⏮` 标记。

这些是实现细节，不影响"延迟重放 + 标记"作为方案成立。**顺序来源 B（server pollLoop 并发）是独立议题**，本次不改（属另一改动范围）。

### 双方收敛

经 R3/R4，双方在主修复（startGen 判定源头落盘）、磁盘多文件、server 幂等去重、延迟重放+标记上**已收敛**。我准备写共识草稿，进入副驾质询。

OK @ 2026-07-15 19:02 Pilot R5

---

## 最终共识

> （本 section 经 R6 质询、R7 修正、R8 确认定稿；R6-R8 原文见下文）

### 根因（完整失败链）

服务端重启后 client 重连"疯狂推微信"，是一条环环相扣的失败链：

1. **`c.send` 是 client 级单例 buffered channel（cap=256），整个 Client 生命周期不重建**（`NewClient` 只 `make` 一次，无 `close`/重新 `make`）。断线时 `writePump` 退出（`WriteMessage` 失败），但生产者（`handleChatInput` goroutine **独立于连接生命周期**）继续写入 → 消息堆积在内存 channel。
2. **断线触发的后台任务结束时，BackgroundResult 走 `select { case c.send <- ...; case <-30s: savePendingResult }`**（`:912-925`）。buffered channel 的非阻塞写入让 30s 超时落盘分支在**典型单 bot 场景下不可达**（单任务产 1 条 BackgroundResult ≪ 256；仅多通道并发边界——断线时 Web UI 活跃挤占 buffer——可达）→ 结果几乎全进 `c.send` 堆积、不落盘。
3. **重连后新 `writePump` 把堆积的 BackgroundResult 全部 flush 给 server**。
4. **server `handleBackgroundResult`（`handler.go:397-427`）直接 `PushMessage(payload.WechatID)`，绕过 attachMap、无幂等去重、无时效校验** → 断线堆积的 N 条结果一次性推到微信 = "疯狂推送"。（普通 ChatMessage/ChatReady 因依赖 attachMap、重启后失效而被静默丢弃，所以只有 BackgroundResult 类被疯狂重发——这正是现象的"选择性"。）
5. **顺序错乱两来源**：A) 断线旧结果与重连新结果在 `c.send`/重放路径按入队时间交错（时间域错乱，非 channel 乱序）；B) server `pollLoop` 并发 `handleMessage`（`wechat.go:259`）导致任务启动顺序不可控。

### 修复方案（按优先级）

**[P0] 主修复 — 断线后台结果源头落盘，不进 `c.send`**
- 位置：`connection.go:912-925`
- 改动：select 之前加 `startGen` 判定——`if atomic.LoadInt64(&c.connGen) != startGen { savePendingResult(payload) } else { c.send <- resultMsg }`
- 复用 `:675` 已记录的 `startGen`，改动极小、无需新字段
- 效果：断线期间的 BackgroundResult 根本不进 `c.send`，重连后只走 `resendPendingResults`（磁盘、可控）单一路径，从源头消除堆积轰炸

**[P0] 配套 — `savePendingResult` 改按 taskID 多文件 / append**
- 当前单文件覆盖式（`:1044`），多条断线结果互相覆盖
- 改为 `pending_results/{clientID}/{taskID}.json` 或 append 数组，`resendPendingResults` 遍历 → 逐个 mark → **成功后删除该文件**

**[P1] 顺序来源 A — 延迟重放 + `⏮` 标记（纯技术方案，不需产品决策）**
- `resendPendingResults` 改为 mark（存内存待推送队列，不立即推）；**pending 队列按 taskID 去重**（mark 时跳过已存在 taskID），**且 mark 时立即删盘**（杜绝多次 Connect 累积重复——R6 问题 1）
- 新任务结果正常送达后，flush 待推送队列，旧结果加 `⏮ [断线前任务结果]` 前缀
- 超时兜底：重连后 N 分钟无新结果，也 flush（避免用户不发新消息时旧结果永远卡住）
- 实现注意：新任务若也走落盘（startGen 不等），"c.send 成功后 flush" 触发点失效，需另设 flush 触发（定时 / resend 一并处理）；多个旧结果按 taskID 顺序统一标记

**[P1] server 兜底 — `handleBackgroundResult` 按 taskID 幂等去重**
- `handler.go:397-427`，维护已处理 taskID 集合（带 TTL），重复的直接丢弃
- 防御 resend（磁盘）与残留 `c.send`（内存）双发等残余边界

**[P2] drain 降级为防御性补充**
- 在 `Connect()` 开头、新 `writePump` 启动**之前** drain 一次 `c.send`，清理已失效的残留（ChatReady/ChatMessage 等，它们因 attachMap 重启清空本就被丢弃）——必须在 writePump 启动前，避免误伤即将发送的消息（R6 补充）
- 非主修复，belt-and-suspenders

### 不在本次范围（独立议题，记录但不处理）
- **顺序来源 B**（server `pollLoop` 并发 `handleMessage` 串行化）：属另一改动
- **in-flight 消息丢失**（`writePump` 取出消息→`WriteMessage` 失败→消息已离 channel 不可恢复）：接受少量丢失，不改
- **follow-up（R6 问题 2）**：`:878-890` 的 10s bgMode 轮询超时后无兜底——当 server `TypeBackgroundMode` 丢失时 `bgActive` 保持 false，BackgroundResult 不产生、结果静默丢失。应在轮询超时后按 connGen 判定落盘，而非丢弃。非本次议题，单列 follow-up。

### 验证要点
- 主场景：client 处理微信任务时 `kill` server → 等 client 重连 → 验证只推 1 条（或按 taskID 的 N 条）结果，不轰炸，旧结果带 `⏮` 标记
- 边界 1：断线时 Web UI 也活跃（多通道挤占 buffer）
- 边界 2：断线期间多任务依次结束（验证 taskID 多文件不互相覆盖）
- 边界 3：重连后用户不发新消息（验证超时兜底 flush）
- 边界 4：server `TypeBackgroundMode` 丢失时（`:878-890` 轮询超时），检查 engine log 确认任务是否静默丢失结果（已有 bug，见 follow-up）

### 待人工确认的实现参数
- 延迟重放超时 N（建议 2-3 分钟）
- `⏮` 标记具体文案
- taskID 幂等去重的 TTL

### 已知 trade-off（R8 确认接受）
- **mark 删盘后、flush 前 crash 窗口**：延迟重放方案下，结果从磁盘删除（mark）到实际推送（flush）之间，若 client 进程崩溃，结果永久丢失（旧行为此窗口更小）。client 崩溃概率低、server 幂等去重在此窗口无法生效（结果从未到达 server），接受为合理 trade-off，不额外处理。

OK @ 2026-07-15 19:12 定稿


---

## R6 - 副驾质询

### 总体

共识草稿覆盖了 R1-R5 讨论的主要收敛点，根因链完整、方案层级清晰。以下质询聚焦 2 个真实问题（非找茬）。

---

### 问题 1：延迟重放的 pending 队列缺少去重约束，与多次 `Connect()` 调用交互存在重复推送风险

共识 P1 说：

> `resendPendingResults` 改为 mark（存内存待推送队列，不立即推）

但 `resendPendingResults` 的调用点在 `Connect():120` —— **每次**重连成功都会调用。考虑一个快速重连场景：

```
Connect#1 → resendPendingResults → 读 disk 文件 → 存 pending 队列 {A}
Connect#2 → resendPendingResults → 读 disk 文件 → 存 pending 队列 {A, A}  ← 重复
... (多次重连)
Connect#N → flush 触发 → 推送 N 条相同 A → 退化为"轰炸"
```

这个场景在实际中可能触发：client 重连后，writePump 刚启动，server 侧 WeChat 的 `GetUpdates` 立刻推来断线期间的消息，client 创建新 `handleChatInput`。如果新任务很快完成、BackgroundResult 成功走 `c.send` → 触发 flush，此时 connect 只发生了一次，问题不大。但如果用户在重连后**没有立即发消息**，client 因网络波动反复重连 N 次，pending 队列会累积 N 份相同 entry。

**修正建议**：共识的"实现注意"部分需显式加上一行：**pending 队列按 taskID 去重**（`resendPendingResults` 从磁盘读取时，跳过已在 pending 队列中的 taskID）。

同时需考虑：如果 `savePendingResult` 正在写文件、`resendPendingResults` 正在读同一个文件，会不会读到半截数据？Go 的 `os.WriteFile` 实现是先 truncate 再 write，有一个极短的空文件窗口。当前代码用 `json.Unmarshal` → 失败则跳过 → 下次 Connect() 再读。在改为多文件后这个窗口仍然存在，但影响降为单条丢失重试、不影响其他 task。可在实现时改用先写临时文件再 rename（原子替换），但非必须。

---

### 问题 2：startGen 判定在 connGen 自检触发的 bgMode 场景下是正确的，但在 server TypeBackgroundMode + 后续断线的复合场景下有一个边界需确认

共识 P0：

> `if atomic.LoadInt64(&c.connGen) != startGen { savePendingResult(payload) } else { c.send <- resultMsg }`

对于"server TypeBackgroundMode 触发 bgMode → 连接未断"场景：`connGen == startGen` → 走 `c.send`。**正确**。

对于"connGen 自检触发 bgMode"场景：`connGen != startGen` → 走落盘。**正确**。

我验证了复合场景（server TypeBackgroundMode 先触发 bgMode → 之后连接断开 → connGen 递增 → 任务完成时判定），结论是 startGen 判定结果正确（走落盘）。这一点草稿没有明确提及，但实现上不需要特殊处理——startGen 比对天然覆盖这个复合场景。

**但有一个值得提及的细节**：`handleChatInput` 在 `streamEnded` 处有一段 10 秒轮询等待 bgMode（`:878-890`）：

```go
if isBot {
    for i := 0; i < 50; i++ {
        c.bgMu.Lock()
        if c.bgMode { c.bgMu.Unlock(); break }
        c.bgMu.Unlock()
        time.Sleep(200 * time.Millisecond)
    }
}
```

这段代码的语义是"等 server 的 TypeBackgroundMode 消息到达"。如果 bgMode 是 connGen 自检设置的（`:758`），bgMode 在进入这段循环前就已经是 true，循环立刻退出，**不影响 startGen 判定**。

但如果 server TypeBackgroundMode **丢失**（server 在发送该消息后立刻崩溃），轮询 10 秒后 bgMode 仍为 false → `bgActive = false` → BackgroundResult **不产生** → 任务结果静默丢失。这是**已有 bug**，不是共识修复引入的，但值得在验证要点中加一条：验证 server TypeBackgroundMode 消息丢失时任务结果的兜底处理（可从 engine 的 log/stderr 判定是否"跑完了但没推送"）。不阻塞本次修复，作为 follow-up 记录。

---

### 其他审查点（无问题，仅记录）

- **P0 `savePendingResult` 多文件改造**：从单文件覆盖改为 `pending_results/{clientID}/{taskID}.json`，`resendPendingResults` 需遍历目录 + 逐个处理 + 成功后删除。并发写/读的竞态（同上）已讨论。无新增问题。
- **P1 server taskID 幂等去重**：TTL 后同一 taskID 可再次通过——如果 resendPendingResults 在 TTL 过期后重发同一结果，server 不会去重。但这属于"防御层有界"的正常设计，超长延迟的重发在 startGen 落盘机制下极少发生。接受。
- **P2 drain 防御**：降级为 belt-and-suspenders，合理。注意 drain 时不要消费掉新 writePump 已开始正常发送的消息——实现上应在 `Connect()` 成功、`writePump` 启动之前 drain（即 `Connect()` 开头），避免误伤。共识没有明确 drain 时机，建议补充"在 writePump 启动之前"。
- **顺序来源 B**：同意独立处理。`pollLoop` 并发 `handleMessage` 串行化涉及 WeChat 消息响应延迟的 trade-off，需单独评估。
- **验证要点**：建议加一条——"重连后若 server TypeBackgroundMode 丢失，检查 engine log 确认任务是否静默丢失结果"，作为防御性回归测试。

---

### 结论

共识草稿在核心方案上无重大问题。上述 2 个问题（pending 去重 + TypeBackgroundMode 丢失）均可通过加一行注释/验证项解决，不改变方案结构。如果主驾同意这两处补充，可定稿。

OK @ 2026-07-15 19:08 Copilot R6

---

## Round 7 - Pilot（质询回应 + 草稿修正）

副驾 R6 质询提了 2 个真实问题 + 2 个补充。**问题 1 采纳（草稿真实遗漏），问题 2 记录为 follow-up，补充全部采纳**。已据此修正"## 最终共识（草稿）"。

### 采纳问题 1：pending 队列去重 + mark 时立即删盘

副驾对。我的 P1 延迟重放方案有个真实漏洞：`resendPendingResults` 每次 `Connect()` 都调用，改为 mark（不立即推）后，若用户重连后不发新消息 + client 因网络波动反复重连 N 次，pending 内存队列会累积 N 份相同 taskID → flush 时推 N 条 → 退化成本 bug 的变体（"延迟版轰炸"）。

修正两处：
1. **pending 内存队列按 taskID 去重**：mark 时跳过已在队列的 taskID。
2. **mark 时立即删盘**：`resendPendingResults` 读盘 → 存内存 pending 队列 → **立即删除磁盘文件**。下次 Connect 不再读到已 mark 的结果，从根上杜绝累积。

（语义变化：`resendPendingResults` 从"读→发→删"变成"读→存内存→删盘→延迟 flush"；flush 成功后清内存队列。）

### 问题 2：已有 bug，记录为 follow-up，不阻塞本次

副驾已验证 startGen 判定对"server TypeBackgroundMode → 后续断线"复合场景正确，无需特殊处理。

其指出的**已有 bug**（`:878-890` 的 10s 轮询等不到 bgMode 时，`bgActive=false` → BackgroundResult 不产生 → 结果静默丢失）确实真实，但**非本次修复引入、非本次议题（疯狂推送）范畴**。采纳为：
- 验证要点加一条：回归"server TypeBackgroundMode 丢失时，检查 engine log 确认任务是否静默丢失结果"。
- follow-up：`:878-890` 轮询超时后应有兜底（超时也按 connGen 判定落盘，而非丢弃）。

### 采纳补充建议

- **P2 drain 时机**：明确为"`Connect()` 开头、新 `writePump` 启动**之前**" drain，避免误伤新 writePump 即将发送的消息。
- **P0 多文件 resend**：明确"`resendPendingResults` 遍历目录 → 逐个 mark → 成功后删除该文件"。

### 下一步

已修正草稿（见下文"## 最终共识（草稿）"），再请副驾确认一次有无新异议，通过后定稿。

OK @ 2026-07-15 19:07 Pilot R7


---

## R8 - 副驾确认

R6 的两个问题均已妥善处理：

1. **Pending 去重 + mark 时立即删盘**（问题 1）：`resendPendingResults` 读盘 → 去重存内存 → 立即删盘。多次 Connect 不再读到旧文件，从源头杜绝累积。✓
2. **TypeBackgroundMode 丢失**（问题 2）：记录为 follow-up + 验证边界 4。startGen 判定在复合场景下已验证正确。✓

补充修正（P2 drain 时机、P0 多文件 resend 后删文件）无新问题。

**一个实现层面需注意的非阻塞细节**：mark 时立即删盘 + pending 队列纯内存，导致"删盘之后、flush 之前"存在一个 crash 窗口——如果 client 进程在这期间崩溃，结果既不在磁盘也不在微信，永久丢失。与旧行为相比，这个窗口确实更大（旧行为中 resendPendingResults 写入 c.send 后 writePump 几乎立即发送；新行为需等下一任务完成或超时）。但这是"延迟重放"方案的固有 trade-off，且 client 进程崩溃概率低、server 端 taskID 去重也无法在此窗口生效（因为结果从未到达 server）。接受为合理 trade-off，不需额外措施。

无新异议，可定稿。

OK @ 2026-07-15 19:12 Copilot R8

---

## 讨论时间线

| Round | 方向 | 主题 | 结果 |
|-------|------|------|------|
| R1 | Pilot | 盲测：现象 + 代码现状（c.send 生命周期、后台模式、BackgroundResult 路由、pending_results） | 锁定主链嫌疑，列 8 个开放问题 |
| R2 | Copilot | 独立分析：30s 超时形同虚设、顺序错乱两来源、writePump in-flight 竞态 | 主链一致，3 点补充 |
| R3 | Pilot | 亮倾向：源头落盘 vs drain；30s 定性为不可达代码 | 与 Copilot 出现分歧 |
| R4 | Copilot | 反驳：startGen 比对比"来源分流"可靠；不可达定性过绝对；延迟重放不需产品决策 | 说服 Pilot，方案收敛 |
| R5 | Pilot | 接受 R4 的 startGen 修正 + 定性修正；部分接受延迟重放 | 双方收敛，准备共识草稿 |
| R6 | Copilot | 质询草稿：pending 队列缺去重（多次 Connect 累积）+ TypeBackgroundMode 丢失 | 2 个真实问题 |
| R7 | Pilot | 采纳问题 1（pending 去重 + mark 删盘），问题 2 记 follow-up，补 drain 时机 / 多文件删除 | 修正草稿 |
| R8 | Copilot | 确认修正无新异议，补充 crash 窗口 trade-off（接受） | 可定稿 |

---

**定稿时间**: 2026-07-15 19:12
**实现归属**: 本 direction 仅讨论分析，不实施代码改动（由调度器统一决定实现归属）
