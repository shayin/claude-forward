# 议题: 服务端重启后 client 重连时微信消息洪泛与顺序错乱

**日期**: 2026-07-15
**参与方**: Pilot (deepseek) × Copilot (zhipu-glm)
**讨论方式**: 单窗口主驾-副驾模式

## 状态
已达成 ✅

---

## Round 1 - Pilot

### 现象描述

服务端（云服务器）重启后，client 重连时微信通道出现消息洪泛——像是断线期间堆积的事件被一次性回放/重发，且消息顺序错乱。

### 代码现状分析

#### 1. 重连循环结构 (`connection.go:160-218`)

`Run()` 是 client 的主循环，连接断开后通过指数退避重连。关键流程：

```
断线 → readPump defer c.cancel() → 旧 writePump 退出 → Disconnect()
→ sleep baseInterval → 新 context → Connect() → 新 readPump/writePump
```

`Connect()` 中（`connection.go:69-122`）：
- 递增 `connGen`（连接代数，line 77）
- 创建新 WebSocket 连接
- 发送 TypeRegister
- 启动新 readPump/writePump
- **调用 `resendPendingResults()`（line 120）**——重发上次断线时未成功发送的后台任务结果

#### 2. `c.send` 通道生命周期 (`connection.go:62`)

`c.send` 在 `NewClient()` 中创建一次（buffered 256），**跨重连不重建**。旧 writePump 退出后，新 writePump 继续从同一个 channel 消费。

**潜在问题**：旧连接断开时，如果 `handleChatInput` goroutine 仍在运行并向 `c.send` 写入消息，这些消息会残留在 buffer 中。新 writePump 启动后会将其发送到新服务器连接。但由于消息的 `To` 字段指向旧的 bot 虚拟连接 ID（服务器重启后已不存在），服务端的 `handleChatClientToUser` 在 `users` map 中查不到目标用户，消息被静默丢弃。这些残留消息本身不会到达微信——但它们会**占据 c.send buffer 空间**，可能阻塞后续正常的消息发送。

#### 3. 多并发 handleChatInput 与 bgMode 竞态 (`connection.go:592-927`)

微信通道的每次 `chatViaHub` 调用都会创建一个虚拟 bot 连接并发 TypeChatInput 给 client。如果用户在服务端宕机期间连续发送了多条消息（被微信缓存），服务端重启后 pollLoop 一次性拉取所有缓存消息，会并发启动多个 `chatViaHub`，进而并发启动多个 `handleChatInput` goroutine。

**关键发现——bgMode 存在竞态条件**：

`bgMode` 是一个单一的 bool 字段（line 51），被多个并发的 `handleChatInput` goroutine 共享：

- **设置阶段**（lines 756-765）：检测到 `connGen != startGen` 时，多个 goroutine 可能同时设置 `bgMode = true`。虽然有 `bgMu` 保护写入，但 check-and-set 不是原子的——每个 goroutine 都独立检测 `!bgActive`，多个 goroutine 可能同时进入该分支。`bgTaskID` 和 `bgWechatID` 的值取决于最后写入者。

- **消费/清除阶段**（lines 891-926）：第一个完成的 goroutine 读取 `bgMode=true`，发送 `BackgroundResult`，然后将 `bgMode` 设为 `false`（lines 896-898）。第二个 goroutine 完成时发现 `bgMode=false`，经过最多 10 秒等待（50×200ms，lines 881-889）后放弃，**其收集的完整文本丢失**。

这解释了"消息顺序错乱"：多个并发任务完成顺序与启动顺序不一致，而只有第一个完成的任务的结果被推送。

#### 4. resendPendingResults 持久化机制 (`connection.go:1044-1087`)

`savePendingResult` 将未成功发送的 BackgroundResult 写入磁盘文件 `~/.claude-forward/pending_results/{clientID}.json`。关键特征：
- **单文件覆盖写**：总是写同一个文件，后写覆盖前写
- **只在 c.send 写入超时（30s）时触发**：正常发送成功不落盘
- **Connect() 时调用 resendPendingResults() 重发**

这意味着：如果多个 `handleChatInput` goroutine 在断线期间完成，且它们都因 `c.send` 阻塞而超时落盘，**只有最后一个落盘的结果会被重发**。这不是洪泛的来源，但会在重连时向微信额外推送一条旧消息。

#### 5. 服务端连接管理 (`hub.go`, `handler.go`)

服务端重启后，Hub 的 `clients`、`users`、`attachMap` 全部清空。client 重连后通过 TypeRegister 重新注册。

**关键路径——client 注销通知**（`hub.go:68-91`）：当旧 client 连接被 unregister 时，Hub 向所有 attach 到该 client 的用户发送 `TypeDetached` 通知。但服务端重启时，旧的 unregister 流程不会执行（服务端进程已死），所以不会有通知。

**虚拟 bot 连接**（`wechat.go:378-527`）：`chatViaHub` 每次调用创建新的虚拟 Connection，Send channel buffered 256。通过 `RegisterBotUser` 直接写入 `users` map（不走 register channel）。服务端重启后，旧的虚拟连接全部消失。

#### 6. 微信消息重投递 (`wechat.go:231-262`)

`pollLoop` 使用长轮询 `GetUpdates()` 获取微信消息。服务端重启后，`WeChatManager.Start()` 重新启动 pollLoop。如果服务端宕机期间有未确认（未 ack）的消息，`GetUpdates()` 可能重新投递这些消息。每个重新投递的消息都会触发一次完整的 `chatViaHub` → `handleChatInput` 流程。

#### 7. 自动后台切换的检测窗口 (`connection.go:675-765`)

`connGen` 变化检测在事件循环内部（line 758），不在循环外部。这意味着：
- 从 `handleChatInput` 启动到第一个事件到达之间，如果发生断线重连（connGen 变化），goroutine **不会立即检测到**
- 在此期间，事件仍会被发送到 `c.send`（因为 `bgActive=false`、`uid != ""`），但这些消息指向旧的 bot 连接 ID，服务端无法投递

### 开放问题

1. **洪泛的消息来源是什么？** 是微信缓存消息的重新投递（服务端 pollLoop 拉取），还是 client 端 `c.send` buffer 中残留的旧消息被回放，还是 `resendPendingResults` 重发的旧结果？还是三者的叠加？

2. **bgMode 竞态丢失的结果去了哪里？** 第二个及后续完成的 `handleChatInput` goroutine 的 `bgFullText` 被丢弃，微信用户收不到这些回复。这是否比洪泛更严重？

3. **connGen 检测是否足够早？** 在 `handleChatInput` 启动和第一个事件到达之间的窗口期内，断线重连无法被及时检测，这个窗口有多长？在实际的 claude 调用中可能长达数秒（进程启动时间）。

4. **为什么消息顺序会错乱？** 多并发 `handleChatInput` goroutine 完成顺序不可预测（取决于每个任务的计算量），微信用户收到的回复顺序与发送顺序不一致。

5. **`chatViaHub` 3 分钟超时 + 30 分钟硬超时是否合理？** 断线期间，client 端的 claude 进程可能仍在运行，但服务端的 `chatViaHub` goroutine 已随进程死亡。重连后重新投递的微信消息会创建新的 `chatViaHub`，而 client 端可能有旧的、针对同一用户消息的 `handleChatInput` goroutine 仍在运行。

OK @ 2026-07-15 12:00 Pilot R1

---

核实完毕。以下是我的 R2 回应。

---

## Round 2 - Copilot

我逐行复核了 Pilot 引用的路径，事实链基本成立，但**根因归位和顺序错乱的发生点需要修正**，并且 Pilot 漏掉了决定洪泛"量级"的一个关键放大器。下面聚焦三个核心点。

### 主张 1：洪泛的放大器是 `handleBackgroundResult` 的零校验直推，而不是 `c.send` 本身——这是单点最高杠杆

Pilot 把 `c.send` 永生 channel 列为"核心嫌疑"。我不同意这个定性。`c.send` 跨重连不重建（`connection.go:62` 全仓确认无 close、无 re-make）是 **Web UI 侧 `sessionEvents` 断线回放的必需设计**（`connection.go:772-788` 的回放路径就靠它），动它会让 Web UI 的重连体验一起崩。channel 永生不是缺陷。

真正的问题在 server 侧的路由不对称，这一点 Pilot d2 开放问题 8 已经摸到，但我认为它应该被**升格为根因**，而不是开放问题：

- `handleBackgroundResult`（`handler.go:397-427`）拿到 payload 后**直接** `h.wechatMgr.PushMessage(payload.WechatID, text)`（`:421`）——不查 `msg.To`、不查 `attachMap`、不校验来源连接是否还活着、不校验 taskID 时效、无去重。
- 对照 `handleChatClientToUser`（`handler.go:466-482`）必须 `for userID, clientID := range h.hub.attachMap` 反查 user，服务端重启后 `attachMap` 全空 → 普通聊天消息**被静默丢弃**。

这条不对称性意味着：c.send 里堆积的任何消息，**只有 `BackgroundResult` 这一类能穿透重启到达微信**，其余全被路由层过滤掉。所以"洪泛的内容"被天然筛选为 `BackgroundResult`。于是修复杠杆极其集中——**只要在 `handleBackgroundResult` 推送前加一道闸门（taskID 携带生成时间戳/connGen，拒绝过期或最近已推过的 taskID），洪泛立刻归零**，不需要动 channel、不需要动 pollLoop、不需要动 bgMode。这是止血优先级 P0。

### 主张 2：顺序错乱发生在 server 入口的并发抢写，不在 client 的 `c.send`——修正 Pilot 的归因

Pilot d2 开放问题 4 把顺序错乱归因于"多个并发 `handleChatInput` 完成顺序不可预测"。这个归因不准确，因为 client 侧对 bot 任务是**硬串行**的：

- `connection.go:623` `if runner.IsRunning()` → 直接回 `TypeChatError` 409 "engine is still processing" 并 `return`（`:624-631`），**不启动新任务**。

也就是说单时刻只有一个 `handleChatInput` 在跑 engine，完成序 ≈ 启动序；而 `BackgroundResult` 是每个任务完成时各写一条进 c.send（`:912-913`），单 channel FIFO 保序。所以 c.send 这一段**并不产生乱序**。

真正的乱序点在 server 入口：

- `wechat.go:258-260` `for _, msg := range msgs { go m.handleMessage(...) }` —— pollLoop **批量 + 并发**。
- 每个 `handleMessage` → 独立 `chatViaHub`（`wechat.go:378`，每次 new 一个 `bot-wechat-<uuid>`）→ 多个 chatViaHub goroutine **并发抢写同一个 `client.Send`**（`chatViaHub:431` `safeSend(client.Send, chatMsg)`）。
- channel 写入顺序由 goroutine 调度决定，**不等于用户发送顺序**。client readPump 串行消费这个被打乱顺序的 TypeChatInput 流，于是回复顺序就乱了。

所以顺序错乱是 **server 侧入口并发**问题，治法是给 `pollLoop` 的 `handleMessage` 加 **per-wechatID 串行队列**（同一用户的消息串行进入 chatViaHub），而不是去动 client 的 c.send 或 bgMode。

### 主张 3（Pilot 完全没提）：`UpdateBuf` 不持久化 → 服务端重启后重投递，这是洪泛"量"的决定因素

Pilot 现象描述里"像是断线期间堆积的事件被一次性回放"——最字面的解释其实不是 c.send 回放，而是 **GetUpdates 重投递**。我 grep 了 `UpdateBuf|saveBuf|loadBuf`：

- `UpdateBuf string` 是 `Bot` 结构体的内存字段（`ilink.go:25`），pollLoop 每轮用它当游标（`wechat.go:256` `user.Bot.UpdateBuf = newBuf`）。
- 全仓**没有 saveBuf/loadBuf**，Bot 在服务端重启时随 `WeChatManager.Start()` 重建，`UpdateBuf` 回到空值。

结论：服务端重启后 pollLoop 用空 buf 重新拉取，**断线期间微信用户发的消息会被重新拉回**。每条重投递消息经情况 A 的时间线（client 此时已重连且空闲）→ chatViaHub → 串行 handleChatInput → 若超时转后台则各产 1 条 `BackgroundResult` → 全部 PushMessage。**洪泛的条数 N ≈ 重投递消息数**，这正是"量"的来源。c.send 堆积（情况 B，断线时正有任务在跑）只是叠加项。

（附带一个 Pilot 也没覆盖的子问题：若重连瞬间 client 还在跑断线前遗留的任务 A，重投递的 B/C/D 的 TypeChatInput 会撞上 `IsRunning()` 全部返回 409 且**不重试** → `chatViaHub` 收到 `TypeChatError` 把"please wait"回给用户，B/C/D 实际**丢失**。这是"丢"不是"洪泛"，应单独治理。）

### 方案倾向（按杠杆排序）

- **P0 止血**：`handleBackgroundResult` 加时效/去重闸门。`BackgroundResultPayload` 已有 `TaskID`，让 client 在 `:904` 生成 taskID 时编入 `connGen` 或时间戳，server 侧 `:421` 推送前拒绝过期（如 >5min）或最近已推过的 taskID。单点修复，洪泛立即收敛，风险最低。
- **P1 治顺序**：`pollLoop` 的 `go m.handleMessage` 改为 per-wechatID 串行队列，消除入口并发抢写，根治乱序。
- **P2 治重投递**：给 `UpdateBuf` 加 `saveBuf/loadBuf`（参照已有的 `savePushQueue/loadPushQueue` 模式，`wechat.go:1059-1088`），重启后游标不回退，从源头消除重投递放大。
- **明确反对**两条：(1) 不要 close/重建 `c.send`——会破坏 Web UI 的 sessionEvents 回放；(2) bgMode 单 bool 竞态（Pilot d2 第 3 节）是真实 bug，但它导致的是"丢结果"而非"洪泛"，优先级低于以上三项，且应作为独立议题，不要和洪泛治理混在同一刀切里。

OK @ 2026-07-15 12:10 Copilot R2

---

## Round 3 - Pilot

副驾的三条主张我逐条回应，然后补充一个副驾分析中被忽略的路径。

### 对主张 1（handleBackgroundResult 零校验）的回应：认同，升格为 P0 根因

副驾提出 `handleBackgroundResult`（handler.go:397-427）与 `handleChatClientToUser`（handler.go:466-482）之间存在路由不对称，这一点我在 R1 中只作为开放问题 5/6 侧面提及，副驾将其定位为"单点最高杠杆"，我完全认同。论证如下：

**代码验证**：handler.go:421 行 `h.wechatMgr.PushMessage(payload.WechatID, text)` 确实不做任何校验——没有查 `attachMap`、没有校验 taskID 时效、没有去重、没有验证来源 client 是否仍然存活。与之对照的 `handleChatClientToUser`（handler.go:466-482）通过 `attachMap` 反查用户，服务端重启后 map 为空，消息被静默滤掉。

**这意味着什么**：c.send 中残留的任何 `TypeBackgroundResult` 消息——无论是断线前已写入但未发出的、还是 `resendPendingResults` 重发的——都会**不受阻碍地穿透重连后的服务器**，直接调用 `PushMessage` 推送到微信。这是所有消息类型中唯一能穿透重启的。这种路由不对称构成了洪泛的"放大器"——client 侧的任何背景结果泄漏都会被 server 侧零校验地放大为微信推送。

**补充证据**：`Connection.go:120` 的 `resendPendingResults()` 在 `Connect()` 中被调用。如果断线前有后台任务完成并将结果持久化到 `pending_results/{clientID}.json`，重连后这个"僵尸结果"会被直接发送到服务器，而 `handleBackgroundResult` 不加阻拦地推送到微信。这不是洪泛的唯一来源，但是时间线上最早到达的——在 WeChat pollLoop 拉取重投递消息之前就已经推送了。

### 对主张 2（pollLoop 并发导致的顺序错乱）的回应：部分认同，但分析不完整

副驾认为 client 侧的 `handleChatInput` 是"硬串行"（因为 `IsRunning()` 检查在 connection.go:623 阻止并发），因此顺序错乱源在 server 的 `pollLoop` 并发 goroutine（wechat.go:258-260）。**我认同 server 入口并发是乱序的根源**，但副驾的"硬串行"分析有一个关键遗漏。

**遗漏**：`IsRunning()` 的 409 拒绝并不等于串行化——它等于**丢弃**。在服务端重启场景下：

1. pollLoop 并发拉取消息 A、B、C（用户发送顺序）
2. 三个 goroutine 并发抢写 `client.Send`
3. 假设 C 的 TypeChatInput 先到达 client，client 启动 `handleChatInput(C)`
4. A 和 B 的 TypeChatInput 随后到达，撞上 `IsRunning() == true`，返回 409 "engine is still processing, please wait"
5. 但 `chatViaHub` 收到 `TypeChatError`（wechat.go:496-501）后直接 `return`——**A 和 B 丢失，不重试**
6. C 完成后发送 BackgroundResult → 用户只收到 C 的回复

所以"入口并发"导致的不只是顺序错乱，还有**消息丢失**。这不是副驾说的"应该单独治理"的次要问题，而是与"顺序错乱"同根的更严重后果。顺序错乱 + 消息丢失是同一个 `pollLoop` 并发模型问题的两面。

**修正建议**：副驾提议的 per-wechatID 串行队列方向正确，但需要补充：串行化后，client 端的 `IsRunning()` 拒绝仍会导致后续消息的 409 丢失。需要在串行队列基础上，client 端加排队等待（而非直接拒绝），或 server 端在 TypeChatError 时做有限次重试。

### 对主张 3（UpdateBuf 不持久化）的回应：认同作为量的决定因素，但需验证 iLink 行为

副驾提出 `UpdateBuf` 不持久化导致服务端重启后用空游标重新拉取消息，这是洪泛"量"的来源。我认同这个方向，但有两点需要验证：

1. **iLink GetUpdates 的行为**：需要确认 iLink API 在收到空 buf 时，是返回"所有未确认消息"还是"从最新消息开始"。如果是前者，所有已在服务端宕机前处理过的消息会被重新拉取并重复处理——这甚至比"断线期间新消息"的量大得多。如果是后者（从最新开始），则只有断线期间新产生的消息会被拉取，洪泛量 = 断线期间的微信消息数。

2. **UpdateBuf 的保存粒度**：副驾建议参照 `savePushQueue/loadPushQueue` 做持久化。但需要注意——`UpdateBuf` 在每轮 pollLoop 后更新（wechat.go:256），如果持久化频率过高会有 IO 性能问题。可以考虑定时批量写或仅在服务端正常关闭时写（`WeChatManager.Stop()` 中）。

### 补充发现：c.send 中残留事件的第二个通路

副驾认为 `c.send` 永生 channel 不是问题，因为 Web UI 的 sessionEvents 回放依赖它。这一点我同意——不能 close/重建 c.send。但副驾的分析忽略了一个关键路径：

**handleChatInput 的 bot 分支在 bgMode 检测之前已经向 c.send 写入了事件**。具体代码路径：

```
connection.go:719-724（bot 分支）:
if uid != "" && !bgActive {
    msg.To = uid
    c.send <- msg    // <-- 在 bgMode 检测之前就写入了
}

connection.go:756-765（bgMode 检测）:
if !bgActive && atomic.LoadInt64(&c.connGen) != startGen {
    c.bgMode = true  // <-- 这里才检测到断线
    ...
}
```

**时序窗口**：connGen 变化检测在事件循环内部（line 758），不在循环外部。从 handleChatInput 启动到第一个事件到达之间，如果发生了断线重连：
- （a）`startGen` 被录为旧值（line 675）
- （b）断线发生，Run() 重连，connGen++（line 77）
- （c）claude 进程产生第一个事件
- （d）事件循环到达 line 758，检测到 `connGen != startGen`
- （e）`bgMode = true`，此后停止写入 c.send

在 (c) 到 (e) 之间，**已经有 N 个事件被写入 c.send**，且 `msg.To` 指向旧的 bot 虚拟连接 ID。这些残留事件在重连后通过新 writePump 发送到服务器。好消息是它们被 `handleChatClientToUser` 的路由检查过滤掉（因为旧 botConn 已不存在）。但如果其中混有 `TypeBackgroundResult`（来自上一个 handleChatInput 的 streamEnded 分支），它就会被 `handleBackgroundResult` 零校验地推送。

**结论**：c.send 永生 channel 本身没问题，但在 bgMode 检测的时序窗口内，bot 分支的 `c.send <- msg` 写入没有被及时阻断。把这个窗口缩到最小（在 connGen 变化的第一时间就停止写入）是防御洪泛的第二道防线。

### R3 综合判断

**根因排序**（按对用户可见影响的贡献度）：

| 优先级 | 根因 | 贡献的症状 |
|--------|------|-----------|
| P0 | `handleBackgroundResult` 零校验路由不对称 | 洪泛（任何来源的 BackgroundResult 都能穿透） |
| P0 | pollLoop 并发 goroutine 模型 | 顺序错乱 + 消息丢失 |
| P1 | UpdateBuf 不持久化 | 洪泛的量（重投递放大） |
| P1 | `resendPendingResults` 无时效校验 | 重连时额外推送僵尸结果 |
| P2 | bgMode 竞态 + 单 bool | 后台结果丢失（非洪泛，但需单独治理） |

**修复策略倾向**：
- 第一刀切在 `handleBackgroundResult`（server 侧，最小改动，最大收益）：加 taskID 时效/去重
- 第二刀切在 `pollLoop`（server 侧，根治顺序+丢失）：per-wechatID 串行队列
- 第三刀：UpdateBuf 持久化（server 侧）+ connGen 窗口缩小（client 侧）
- bgMode 竞态作为独立 bug 单独治理

OK @ 2026-07-15 12:20 Pilot R3

---

验证完毕。两个新论点都有了确定的代码答案，其中一个（iLink 游标语义）需要修正我们共同的假设边界。以下是 R4。

---

## Round 4 - Copilot

R3 之后双方在三大根因上已对齐，我这轮做三件事：**证伪一个被夸大的独立通路、修正"量"的真实边界、简化 P1 的实现**，推动收敛到最小可执行修复集。

### 点 1：Pilot 的"第二通路"不成立——这反而坐实了"P0 是充分必要的一刀"

Pilot R3「补充发现」担心 `connection.go:719-724` 的 `c.send <- msg` 在 bgMode 检测窗口（`:756-765`）之前执行，泄漏事件到 c.send。我复核了这段代码：`:700` 构造的是 `protocol.NewMessage(protocol.TypeChatMessage, ...)`，`:719-724` bot 分支转发的就是这条 **TypeChatMessage**，`msg.To = uid`（旧 botConn ID）。Pilot 自己也承认"好消息是它们被 `handleChatClientToUser` 过滤掉"。

关键反驳在 Pilot 的后半句——"如果其中混有 `TypeBackgroundResult`（来自上一个 handleChatInput 的 streamEnded 分支）"。这个"如果"在代码上不成立：

- `BackgroundResult` 的唯一写入点是 `streamEnded` 段（`:904-913`），在事件循环 `for{}` **退出之后**执行，和 `:719-724` 的事件转发是互斥的代码段，不存在"混入"。
- 进入 `streamEnded` 时，`bgMode` 已在循环内**最后一个 event** 的 `:758` 检测过。窗口泄漏的 TypeChatMessage 和 BackgroundResult 走的是两条物理上不重叠的路径。

所以 Pilot 的第二通路里能泄漏的是 TypeChatMessage（被路由过滤，不穿透），而非 BackgroundResult。**这恰恰强化了我们的共识**：穿透重启的消息类型**只有 BackgroundResult 一种**，无论它来自 c.send 堆积、`resendPendingResults`（`:120`/`:1063`）还是 `streamEnded` 正常完成，都汇入同一个 `handleBackgroundResult`（`handler.go:421`）入口。

推论：**P0（在 `handleBackgroundResult` 加 taskID 时效/去重闸门）是充分必要的一刀**，覆盖所有已知的 BackgroundResult 来源，包括 Pilot 担心的时序窗口里"假如"泄漏的那条。Pilot 提的"缩小 connGen 检测窗口"作为**纵深防御**是合理的（减少无意义的 c.send 垃圾写入），但它不构成独立的洪泛路径，不应在根因表里单列——否则会让人误以为 P0 之外还有第二条止血线要同时做。

### 点 2：iLink 游标语义——重投递真实存在，但"量"的上限受平台约束，需实测确认

Pilot R3 点 1 提的"iLink 空 buf 行为需验证"非常关键，因为我和 Pilot 主张 3 的整个"量"都压在它上面。我读了 `GetUpdates` 实现（`ilink.go:228-275`），有确定结论也有边界：

**确定的部分**：
- `:230-233` 请求带 `get_updates_buf`，`:249-252` 服务端（iLink 平台）返回**新的** `resp.GetUpdatesBuf`。游标的**真相在 iLink 平台侧**，`b.UpdateBuf` 只是 claude-forward 持有的副本。
- 全仓**没有独立的 ack 调用**——游标推进（下次请求带新 buf）就是唯一的确认机制。claude-forward 没有 `saveBuf`，Bot 随 `WeChatManager.Start()` 重建，`UpdateBuf` 回空。

由此可严格推出：**服务端重启 = 游标副本丢失 = 下次空 buf 请求 = iLink 平台重新返回"未被新 buf 确认过"的消息**。重投递是必然的，方向上 Pilot 和我都没错。

**不确定的边界**（必须实测）：
- 空 buf 请求时，iLink 返回的是"全部历史"还是"近期未读"？这决定洪泛上限。代码里看不出来，取决于 iLink 平台的保留策略。
- 我的倾向判断是**"断线期间新消息"量级**（Pilot R3 点 1 的第二种情况），而非全量历史——否则系统每次常规部署重启都会洪泛，与"服务端重启后"才触发的现象描述不符。但这只是推断，**P2 落地前必须实测一次空 buf getupdates 的返回量**，否则可能修复了一个没那么严重的问题、却低估了别的。

这对修复顺序有影响：P0/P1 不依赖这个答案（它们治"穿透"和"乱序/丢失"，与量无关），可以先做；P2（UpdateBuf 持久化）的优先级和实现粒度（Pilot R3 点 2 提的 IO 频率问题）应该**等实测结果再定**——我同意 Pilot 说的"定时批量写或 `Stop()` 时写"，避免每轮 pollLoop 都落盘。

### 点 3：简化 P1——串行队列的释放时机正确，就能一并消除 409 丢失，无需 client 排队或 server 重试

Pilot R3 点 2 接受了我的 per-wechatID 串行队列方向，但追加了一个复杂化：认为串行化后 client 端 `IsRunning()` 仍会 409 丢消息，需要"client 排队等待"或"server 有限次重试"。我认为这是多虑，正确的串行队列设计**根本不会触发 409**：

- per-wechatID 串行队列保证：同一用户的消息按发送序**依次**进入 `chatViaHub`，前一条的 chatViaHub **未 return 前**，下一条不投递。
- chatViaHub 的 return 点有三个：任务完成（`:494` ChatReady）、超时转后台（`:513`）、错误（`:501`）。**任一 return 后队列立即投递下一条**。
- 因为 server 串行投递，client 端在前一条 `handleChatInput` 结束前**收不到**下一条 TypeChatInput → `IsRunning()` 永远为 false → **409 路径根本不被触发**。

所以 Pilot 担心的 A/B 丢失场景（`:623` 409）在串行队列下自动消失，不需要额外的 client 排队或 server 重试逻辑——那会引入新的复杂度和重试风暴风险。**P1 一刀同时根治"顺序错乱"+"消息丢失"两个症状**，这正是它值得列为 P0 同级的原因。

唯一代价：长任务转后台时，chatViaHub 要等 3 分钟超时（`:446`）才 return，队列里后续消息累积 ≤3min 延迟。对"断线重投递"这种异常连发场景，3 分钟延迟远优于"丢失"，可接受。若日后要优化，可让 client 在 `:762` 切后台时主动发一个"已转后台"信号让 chatViaHub 提前 return——但这是 nice-to-have，不阻塞 P1 止血。

### 收敛：最小可执行修复集

| 优先级 | 动作 | 位置 | 解决症状 | 依赖 |
|--------|------|------|---------|------|
| P0 | `handleBackgroundResult` 加 taskID 时效/去重闸门（taskID 编入时间戳或 connGen） | `handler.go:421` + `connection.go:904` | 洪泛（所有来源） | 无，可立即做 |
| P0 | pollLoop 改 per-wechatID 串行队列，chatViaHub return 即释放 | `wechat.go:258-260` | 顺序错乱 + 消息丢失 | 无，可立即做 |
| 待定 | 实测 iLink 空 buf getupdates 返回量 | — | 确定 P2 优先级 | 阻塞 P2 决策 |
| P1/P2 | UpdateBuf 持久化（`Stop()` 时写 + 启动时 load） | 新增 saveBuf/loadBuf | 重投递放大 | 依赖实测结果 |
| 纵深 | 缩小 connGen 检测窗口（非独立根因） | `connection.go:756-765` | 减少 c.send 垃圾写入 | 可选，不阻塞止血 |
| 独立 | bgMode 单 bool 竞态 | `connection.go:756-765,891-900` | 后台结果丢失（非洪泛） | 单独议题 |

**共识**：P0 两刀（BackgroundResult 闸门 + 串行队列）是止血核心，覆盖"洪泛+乱序+丢失"三大用户可见症状，且互不依赖、可并行开发。**剩余分歧已收敛为实测问题**（iLink 返回量），不再是设计分歧。建议 P0 两刀落地后再看是否还需要 P2。

OK @ 2026-07-15 12:30 Copilot R4

---

## Round 5 - Pilot（共识草稿）

四轮讨论后，双方在根因认定和修复策略上已高度对齐。R4 副驾证伪了我的"第二通路"担忧（TypeChatMessage 与 BackgroundResult 在代码上互斥），同时简化了 P1 实现（串行队列 + chatViaHub return 释放即可自动消除 409）。以下为共识草稿，提交副驾质询。

---

## 最终共识（草稿）

### 问题诊断

**现象**：服务端重启 → client 重连 → 微信通道消息洪泛 + 顺序错乱 + 部分消息丢失。

**三大根因**：

| # | 根因 | 位置 | 机制 |
|---|------|------|------|
| R1 | `handleBackgroundResult` 路由不对称 | `handler.go:397-427` | 零校验直推 `PushMessage`，不查 attachMap、不验 taskID 时效、不去重。`BackgroundResult` 是唯一能穿透服务端重启的消息类型。 |
| R2 | `pollLoop` 并发 goroutine 模型 | `wechat.go:258-260` | `go m.handleMessage` 批量并发 → 多个 `chatViaHub` 抢写 `client.Send`，顺序由调度决定 ≠ 用户发送序。且前一条未完成时后续消息撞 409 丢失。 |
| R3 | `UpdateBuf` 不持久化 | `ilink.go:25` + `wechat.go:256` | 服务端重启 → Bot 重建 → 游标归零 → iLink 重新投递断线期间的未确认消息 → 洪泛的"量"被放大。 |

### 修复策略（优先级排序）

#### P0-1：`handleBackgroundResult` 加 taskID 时效/去重闸门

- **位置**：`handler.go:397-427`（server 侧）+ `connection.go:904`（client 侧 taskID 编码）
- **方案**：client 在 `connection.go:904` 生成 `BackgroundResultPayload.TaskID` 时编入 `connGen` 值或 UnixMilli 时间戳（如 `"auto-bg-{connGen}-{now}"`）。server 在 `handler.go:421` 推送前增加校验：
  - 解析 taskID 中的时间戳，拒绝超过 N 分钟（建议 5 分钟）的过期任务
  - 维护最近推送的 taskID 集合（LRU，容量 100），拒绝重复推送
- **收益**：切断所有来源的 BackgroundResult 穿透，单点修复洪泛
- **风险**：极低，仅增加 server 侧内存 map + taskID 解析

#### P0-2：`pollLoop` 改为 per-wechatID 串行队列

- **位置**：`wechat.go:258-260`（server 侧）
- **方案**：
  - 在 `wechatUserState` 中增加 `msgQueue chan ILinkIncomingMessage`（buffered 64）
  - 新增 `processLoop` goroutine（per-wechatID），串行消费队列中的消息
  - `pollLoop` 中 `go m.handleMessage` 改为 `msgQueue <- msg`（非阻塞写，满则打日志丢弃）
  - 串行队列保证前一条 `chatViaHub` return 后才投递下一条 → `IsRunning()` 永远为 false → 409 自动消除
- **收益**：同时解决顺序错乱 + 消息丢失
- **代价**：长任务转后台时有 ≤3min 队列延迟（chatViaHub 超时才 return），对异常连发场景可接受
- **风险**：低。需注意 queue 满时的丢弃策略 + 日志

#### 待实测决定优先级

- **P2：UpdateBuf 持久化**
  - 方案：在 `WeChatManager.Stop()` 中 `saveBuf`，启动时 `loadBuf`
  - 阻塞项：需实测 iLink 空 buf `GetUpdates` 的返回量（全部历史 vs 近期未读）
  - 建议 P0 止血后实测再定优先级

#### 纵深防御（可选，不阻塞止血）

- 缩小 connGen 检测窗口：将 `connection.go:756-765` 的检测提前到事件循环之前（如 claude 进程启动后立即检查一次）
- 收益：减少无意义的 `c.send` 垃圾写入

#### 独立议题

- bgMode 单 bool 竞态（`connection.go:756-765,891-900`）：导致后台结果丢失而非洪泛，作为独立 bug 单独治理

### 实现顺序建议

1. P0-1 + P0-2 并行开发（互不依赖）
2. 部署验证 P0 效果
3. 实测 iLink 空 buf 返回量，决定 P2 优先级
4. P2 + 纵深 + bgMode 竞态，按各自排期

OK @ 2026-07-15 12:40 Pilot R5（共识草稿）

---

我逐条审查了共识草稿，R1/R2/R3 根因表、P0-1 的闸门思路、纵深防御和独立议题的划分都没有异议。但 P0-2 和 P0-1 各有一个真实成立的硬伤，其中一个是我自己在 R4 引入、草稿照单全收的错误论断，必须收回。

## 质询 1（核心）：P0-2「串行队列 → IsRunning 永远为 false → 409 自动消除」在「转后台」场景下不成立——草稿继承了我在 R4 的错误

草稿 P0-2 写：「串行队列保证前一条 `chatViaHub` return 后才投递下一条 → `IsRunning()` 永远为 false → 409 自动消除」，收益列为「同时解决顺序错乱 + 消息丢失」。

这个论断只对了一半。它依赖一个隐藏前提：**chatViaHub return 时，client 端的 `handleChatInput` 已经结束、`IsRunning()` 已释放**。该前提在「任务正常完成」路径成立（chatViaHub 收到 ChatReady `:494` 时，client 的 handleChatInput 确实已结束），但在**转后台路径不成立**：

- chatViaHub 的 3 分钟超时分支（`:504-513`）return 时，client 侧 `handleChatInput` **仍在运行**——超时是因为 client 3 分钟没发事件，而 claude 进程还活着在算，`runner.IsRunning()` 仍为 true。
- 此刻队列投递下一条 B → client 收到新 TypeChatInput → `connection.go:623` `if runner.IsRunning()` 命中 → 返回 409 → **B 丢失**。

所以 P0-2 在最高频的 bug 场景（断线重连后 claude 任务普遍较长、易触发转后台）下，**消息丢失根本没被解决**。草稿的「代价是 ≤3min 队列延迟」描述也错了：B 不是延迟 3 分钟，而是被 409 永久拒绝、不重试、直接丢失。延迟和丢失是两回事。

必须诚实说明：**这恰恰证明 Pilot R3 的「client 排队或 server 重试」不是多虑，是必要的**。我 R4 点 3 把它斥为「复杂化」是错的，需要收回。要让 P0-2 真正消除丢失，二选一：
- (a) client 端把"转后台但未完成"期间的后续 TypeChatInput **排队**（而非 409 拒绝），等当前任务真正结束后再处理；或
- (b) server 串行队列的释放点不能是"chatViaHub return"，而要等一个"client 任务真正结束"的信号（目前不存在，需新增）。

建议把 P0-2 的收益从「解决消息丢失」改写为「解决顺序错乱；消息丢失仅在任务不转后台时消除，转后台场景仍需配套 (a)/(b)」，否则会误导实现者以为一刀就够了。

附带：草稿「queue 满（64）则丢弃」的策略也会在重投递洪泛时重新引入丢失，与「解决消息丢失」的收益声明自相矛盾——即便释放时机修对，这条也要在收益里如实标注边界。

## 质询 2：P0-1 的时效校验依赖 taskID 统一编码，但草稿改错了位置——`connection.go:904` 是消费点不是生成点，且 taskID 有多个格式不一的生成点

草稿 P0-1 写：「client 在 `connection.go:904` 生成 `TaskID` 时编入 connGen 或时间戳」。

复核代码：`:904` 用的是 `bgTask := c.bgTaskID`（`:893` 取值），`:904` 是**消费** `c.bgTaskID`，不是生成点。`c.bgTaskID` 的实际赋值点至少有三处，格式互不相同：

1. `connection.go:760` 自动后台切换：`"auto-bg-{UnixMilli}"`（有时间戳，无 connGen）
2. server 侧 `chatViaHub` 超时/硬超时（`wechat.go:506,517`）：`uuid.New().String()`，经 `BackgroundModePayload.TaskID` 传给 client 存为 `c.bgTaskID`——**纯 uuid，无时间戳无 connGen**
3. `connection.go:829` streamEnded 的 error 分支：`"error-{UnixMilli}"`，且这条**绕过 `c.bgTaskID`**，直接构造 resultMsg

草稿只改「904」，实际上 904 不生成 taskID；而路径 2 的 uuid taskID 是 server 生成、client 原样回传的，client 端无法给它"编入时间戳"。结果：server 侧 `:421` 解析 taskID 时间戳时，对 uuid 格式要么解析失败误拒（误杀正常后台结果），要么跳过校验放过（闸门形同虚设）。

要让 P0-1 真正生效，必须**统一所有 taskID 生成点的编码格式**（含 server 侧 `wechat.go:506/517` 的 uuid 生成），让每个 taskID 都自带可解析的时间戳/connGen，而不是只动 client 的 904。建议草稿把 P0-1 的"位置"一行改为覆盖全部生成点，或干脆改用独立字段（在 `BackgroundResultPayload` 里新增 `CreatedAt int64` / `ConnGen int64`，client 生成 resultMsg 时统一填入），避开 taskID 字符串解析的脆弱性。

---

以上两点都是实现层面会直接导致"P0 止血失败"的硬伤，建议修订草稿后再进入实现。其余诊断与策略我独立复核后无异议。

OK @ 2026-07-15 12:50 Copilot R5

---

## Round 6 - Pilot（修订共识草稿）

接受副驾质询的两条硬伤，逐条修订。

### 对质询 1 的回应：接受，修正 P0-2 收益声明 + 补充配套方案

副驾正确地指出：chatViaHub 超时 return ≠ client 任务结束。在转后台路径下 `IsRunning()` 仍为 true，下一条消息仍撞 409。**我收回 R4 中对此的认同**，并将 P0-2 的收益声明从「解决消息丢失」修正为「解决顺序错乱，转后台场景的消息丢失需配套方案」。

配套方案选择 (a)：**client 端排队而非拒绝**。理由：
- 不需要新增协议信号（方案 b 需要新增"任务结束"信号，跨层改动大）
- 改动集中在 `connection.go:623-631` 的 409 分支：将"直接拒绝"改为"将 TypeChatInput 的 text 存入 pending 队列"
- 当前 `handleChatInput` 的 `streamEnded` 段增加：完成后检查 pending 队列，若有待处理消息则递归调用 `handleChatInput`

修正后的 P0-2：

```
P0-2: pollLoop 改 per-wechatID 串行队列 + client 端 pending 队列
- Server: wechat.go:258-260 改为串行队列
- Client: connection.go:623-631 的 409 分支改为 pending 排队（buffer 8）
- 串行队列解决顺序错乱
- pending 排队解决转后台场景的消息丢失
- 边界：pending 满时记录日志并丢弃（极端情况，远优于当前全量丢失）
```

### 对质询 2 的回应：接受，改用独立字段

副驾正确地指出 taskID 多生成点 + 编码格式不统一的问题。采用其建议的**独立字段**方案——比解析 taskID 字符串更可靠。

修正后的 P0-1：

```
P0-1: handleBackgroundResult 加 taskID 时效/去重闸门
- 协议层：BackgroundResultPayload 新增 CreatedAt int64 (UnixMilli) 字段
- Client: connection.go 所有构造 BackgroundResultPayload 的位置统一设置 CreatedAt = time.Now().UnixMilli()
  - 自动后台切换路径 (:904-911)
  - error 分支 (:828-835)
- Server: handler.go:421 推送前校验
  - CreatedAt 距今 > 5min → 拒绝并日志
  - 维护最近 N 个 taskID 的 CreatedAt 集合（LRU），拒绝重复
```

### 修订后的共识终稿

---

## 最终共识（修订版，待副驾确认）

### 问题诊断

**现象**：服务端重启 → client 重连 → 微信通道消息洪泛 + 顺序错乱 + 部分消息丢失。

**三大根因**：

| # | 根因 | 位置 | 机制 |
|---|------|------|------|
| R1 | `handleBackgroundResult` 路由不对称 | `handler.go:397-427` | 零校验直推 PushMessage。BackgroundResult 是唯一能穿透服务端重启的消息类型。 |
| R2 | `pollLoop` 并发 goroutine 模型 | `wechat.go:258-260` | 批量并发抢写 client.Send，顺序 ≠ 用户发送序。前一条未完成时后续消息撞 409 丢失。 |
| R3 | `UpdateBuf` 不持久化 | `ilink.go:25` + `wechat.go:256` | 服务端重启 → Bot 重建 → 游标归零 → iLink 重新投递断线期间未确认消息 → 洪泛量放大。 |

### 修复策略

#### P0-1：`handleBackgroundResult` 加时效/去重闸门

- **位置**：`handler.go:397-427`（server）+ `connection.go` 所有 BackgroundResultPayload 构造点（client）
- **改动**：
  - 协议层：`BackgroundResultPayload` 新增 `CreatedAt int64`（UnixMilli）
  - Client：所有构造点统一设置 `CreatedAt = time.Now().UnixMilli()`（包括自动后台路径 `:904-911`、error 分支 `:828-835`、以及 `resendPendingResults` 重发的 payload）
  - Server `handler.go:421` 前：拒绝 `CreatedAt` 距今 > 5min 的任务；维护 LRU 去重集合（最近 100 个 taskID）
- **收益**：切断所有来源的 BackgroundResult 穿透 → 洪泛止血
- **风险**：极低。LRU map + 时间戳整数字段，无字符串解析脆弱性

#### P0-2：`pollLoop` 串行队列 + client pending 队列

- **位置**：`wechat.go:258-260`（server）+ `connection.go:623-631`（client）
- **改动**：
  - Server：`wechatUserState` 新增 `msgQueue chan ILinkIncomingMessage`（buffered 64）；pollLoop 中 `go m.handleMessage` 改为串行 goroutine 消费队列
  - Client：`connection.go:623-631` 409 分支从"直接拒绝"改为 pending 排队（buffer 8）→ handleChatInput 完成后检查并递归处理
- **收益**：
  - 顺序错乱：✅ 根治（串行投递保证顺序）
  - 消息丢失（正常完成路径）：✅ 根治（chatViaHub return 时 IsRunning 已释放）
  - 消息丢失（转后台路径）：✅ 缓解（pending 队列兜底，满时丢弃而非全量丢失）
- **边界**：pending 满（8）时丢弃 + 日志告警；转后台场景下后续消息有 ≤3min 延迟（chatViaHub 超时 + pending 等待）
- **风险**：低。需注意 pending 队列递归深度控制（建议递归上限 5 层）

#### 待实测决定优先级

- **P2：UpdateBuf 持久化**：`WeChatManager.Stop()` 落盘 + 启动时 load。阻塞于 iLink 空 buf `GetUpdates` 返回量实测。

#### 纵深防御（可选）

- 缩小 connGen 检测窗口（`connection.go:756-765` 提前到事件循环前）

#### 独立议题

- bgMode 单 bool 竞态（`connection.go:756-765,891-900`）：导致后台结果丢失，非洪泛

### 实现顺序

1. P0-1 + P0-2 并行开发
2. 部署验证
3. 实测 iLink 空 buf 返回量 → 决定 P2 优先级
4. P2 + 纵深 + bgMode 竞态按各自排期

---

以上为修订后的共识终稿。副驾上次质询指出的两个硬伤已全部修正。请副驾对修订版做最终确认。

OK @ 2026-07-15 13:00 Pilot R6（修订版）

---

我逐条复核了修订版。P0-1 改用 `CreatedAt` 独立字段并覆盖三个构造点、P0-2 收益声明的修正、server 串行队列方向、根因表——这些都没有异议，两条旧硬伤确实修掉了。但修订在"加 pending 队列"和"加 CreatedAt 时效"两处新引入了两个真实成立的新问题，其中一个是会把后台结果推给**错误微信用户**的正确性 bug。

## 质询 1（正确性）：P0-2 的 pending 队列「只存 text」会丢失消息归属 → currentWechatID 串扰 → 后台结果推给错误用户

修订版第 438 行写「将 TypeChatInput 的 **text** 存入 pending 队列」，第 503 行同样只提 pending 排队 + 递归处理。只存 text 是不够的——`TypeChatInput` 的 payload 还携带 `WechatID` 和 `From`（botConn ID `bot-wechat-<uuid>`），而 client 端的后台结果投递依赖 `c.currentWechatID`（client 级单值，`connection.go:452-457` 在收到 TypeChatInput 时设置，`:761` 自动后台时用作 `bgWechatID`）。

串扰路径：转后台期间多个微信用户的消息进入同一个 pending 队列（client 只有一个 engine，`IsRunning` 是 engine 级，不同 wechatID 的消息会撞同一个 409 分支），队列里混入 X、Y 两个用户的消息 text。递归处理时：
- 取出 pending 项只有 text，**没有 WechatID** → 无法恢复 `c.currentWechatID` 到正确值；
- 即便能跑完，`:761` 记录的 `bgWechatID` 取自当时已被污染的 `c.currentWechatID`；
- 结果：用户 X 的后台任务结果被 `PushMessage` 推给用户 Y。

这正是 d1 讨论里就标出的 `currentWechatID` 串扰风险，pending「只存 text」会把它从"理论可能"变成"必现"。这比洪泛更严重——洪泛是量的问题，推错人是隐私/正确性问题。

修复方向：pending 队列项必须存完整上下文 `{Text, WechatID, From}`，递归 `handleChatInput` 时用存的 WechatID 重新设置 `currentWechatID`，而非依赖单值副作用。建议草稿把第 438/503 行的"text"明确改为"完整 TypeChatInput 上下文"。

## 质询 2（逻辑冲突）：P0-1 的 5min 时效校验与 `resendPendingResults` 的「延迟重发」设计意图直接冲突，resend payload 的 CreatedAt 取值未定义

修订版第 493 行把 `resendPendingResults` 重发的 payload 也列入"统一设置 CreatedAt"，但没说取什么值——而这恰恰是个死结：

- **若取落盘时的原始时间**（= 任务完成时间）：`resendPendingResults` 的整个存在意义就是"断线期间没发出去的结果，重连后补发"，而**断线超过 5min 正是它的核心场景**。此时原始 CreatedAt 距今 >5min，会被 `:421` 的时效校验拒绝 → 合法后台结果永久丢失，resend 机制被 P0-1 架空。
- **若覆盖为重发当前时间**（`time.Now()`）：时效校验对它形同虚设，且这条落盘的僵尸结果会在**每次重连**都被重发一次（LRU 只在 server 内存，server 重启即清空），重新引入洪泛——正是 P0-1 要消灭的东西。

两种取值都有副作用，修订版都没有处理。这暴露一个未被回答的产品决策：**断线 >5min 的后台任务结果，到底要不要送达微信？**
- 要送达 → resend 的结果必须豁免 5min 时效（如 payload 加 `IsResend bool` 标记，server 对标记豁免时效但保留 LRU 去重）；
- 不要送达（止血优先）→ 那 `resendPendingResults` 这个机制本身就该废弃，而不是既保留又被时效拦截，左右不是。

建议在终稿里明确这个产品决策并落到 P0-1 的校验规则里，否则实现者会二选一踩坑。

---

其余部分（根因表、P0-1 独立字段方案、P0-2 串行队列、P2 待实测、纵深与独立议题划分）我独立复核后无异议。这两条新硬伤修复后，我认为终稿可以进入实现。

OK @ 2026-07-15 13:10 Copilot R6

---

## Round 7 - Pilot（最终修订 + 确认）

接受副驾质询的两条新硬伤。逐条修改。

### 对质询 1 的回应：接受，pending 存完整上下文

将 P0-2 的 pending 项从 `string`（text）改为 struct `{Text, WechatID, From}`：

```
P0-2 修订：
- pending 队列项类型: struct { Text string; WechatID string; From string }
- 递归处理时用存的 WechatID 恢复 c.currentWechatID
- handleChatInput 签名改为接受 pending 上下文而非裸 text
```

### 对质询 2 的回应：产品决策 + 技术方案

**产品决策**：断线期间完成的后台任务结果**应当送达微信**。理由：用户发送了消息并期望回复，claude 确实完成了计算并产生了结果，只是因为传输层断开而无法即时送达。这种情况下丢掉结果是"假阳性丢失"——用户以为系统在工作（因为最终没收到错误提示），实际上结果被丢弃了。

**技术方案**：`BackgroundResultPayload` 新增 `IsResend bool`：
- `resendPendingResults` 读取磁盘 payload 时设置 `IsResend = true`
- Server `handler.go:421` 校验规则：
  - `IsResend == true`：跳过 5min 时效校验，仅做 LRU 去重（同一 taskID 在 server 当前生命周期内不重复推送）
  - `IsResend == false`：正常 5min 时效 + LRU 去重

**为什么 LRU 去重对 resend 够用**：`resendPendingResults` 每次重连只发送一份 payload（单文件覆盖写），发送成功后删除文件。LRU 去重防止的是：同一个 server 生命周期内，resend 消息被写入 c.send → writePump 发送 → server 处理 → 推送微信 → 完成。如果 writePump 发送成功但 client 在收到 ack 前又断开，文件未被删除，下次重连又会重发。此时 server 的 LRU（按 taskID 去重）拦截。

**修正后的 P0-1**：

```
P0-1: handleBackgroundResult 加时效/去重闸门
- 协议: BackgroundResultPayload 新增 CreatedAt int64, IsResend bool
- Client: 所有构造点设置 CreatedAt = time.Now().UnixMilli()
  - 自动后台 (:904-911): CreatedAt=now, IsResend=false
  - error (:828-835): CreatedAt=now, IsResend=false
- Client resendPendingResults (:1063-1087): 读磁盘 json → 设置 IsResend=true（不覆盖 CreatedAt）
- Server handler.go:421 前:
  - 非 resend: CreatedAt > 5min → 拒绝 + 日志
  - 所有: taskID LRU 去重（最近 100 个），重复 → 拒绝 + 日志
```

---

## 最终共识

### 问题诊断

**现象**：服务端重启 → client 重连 → 微信通道消息洪泛 + 顺序错乱 + 部分消息丢失。

**三大根因**：

| # | 根因 | 位置 | 机制 |
|---|------|------|------|
| R1 | `handleBackgroundResult` 路由不对称 | `handler.go:397-427` | 零校验直推 PushMessage。BackgroundResult 是唯一能穿透服务端重启的消息类型。 |
| R2 | `pollLoop` 并发 goroutine 模型 | `wechat.go:258-260` | 批量并发抢写 client.Send，顺序 ≠ 用户发送序。前一条未完成时后续消息撞 409 丢失。 |
| R3 | `UpdateBuf` 不持久化 | `ilink.go:25` + `wechat.go:256` | 服务端重启 → Bot 重建 → 游标归零 → iLink 重新投递断线期间未确认消息 → 洪泛量放大。 |

### 修复策略

#### P0-1：`handleBackgroundResult` 加时效/去重闸门

- **位置**：`handler.go:397-427`（server）+ `connection.go` 所有 `BackgroundResultPayload` 构造点（client）
- **协议变更**：`BackgroundResultPayload` 新增 `CreatedAt int64`（UnixMilli）、`IsResend bool`
- **Client 改动**：
  - 自动后台路径（`:904-911`）：`CreatedAt=time.Now().UnixMilli()`, `IsResend=false`
  - Error 分支（`:828-835`）：同上
  - `resendPendingResults`（`:1063-1087`）：读磁盘 json → 设 `IsResend=true`（保留原始 `CreatedAt`）
- **Server 校验**（`handler.go:421` 前）：
  - 非 resend：`CreatedAt` 距今 > 5min → 拒绝 + 日志
  - 所有类型：taskID LRU 去重（容量 100）→ 重复则拒绝 + 日志
  - Resend 豁免时效校验（保留 LRU 去重），因为 resend 的设计意图就是补发断线期间完成的结果
- **产品决策**：断线 >5min 的后台任务结果**应当送达**（用户已发消息、claude 已完成计算，不应因传输层断线而丢结果）
- **收益**：切断所有来源的 BackgroundResult 穿透 → 洪泛止血
- **风险**：极低。独立整数字段，无字符串解析脆弱性

#### P0-2：`pollLoop` 串行队列 + client pending 队列

- **位置**：`wechat.go:258-260`（server）+ `connection.go:623-631`（client）
- **Server 改动**：
  - `wechatUserState` 新增 `msgQueue chan ILinkIncomingMessage`（buffered 64）
  - 新增 `processLoop` goroutine（per-wechatID），串行消费 `msgQueue`
  - pollLoop 中 `go m.handleMessage` 改为 `msgQueue <- msg`（非阻塞写，满则日志 + 丢弃）
- **Client 改动**：
  - `connection.go:623-631` 的 409 分支从"直接拒绝"改为 pending 排队（buffer 8）
  - Pending 项类型：`struct { Text string; WechatID string; From string }`（**完整上下文，避免 WechatID 串扰**）
  - 递归处理时用 pending 项中的 `WechatID` 恢复 `c.currentWechatID`
  - `handleChatInput` 完成后检查 pending 队列，递归上限 5 层
- **收益**：
  - 顺序错乱：✅ 根治（per-wechatID 串行投递）
  - 消息丢失（正常完成路径）：✅ 根治（chatViaHub return 时 IsRunning 已释放）
  - 消息丢失（转后台路径）：✅ 缓解（pending 兜底，满时丢弃 + 日志）
- **边界**：pending 满（8）→ 丢弃 + 日志告警；转后台场景后续消息 ≤3min 延迟
- **风险**：低。pending 递归深度受控（上限 5），WechatID 不串扰

#### 待实测决定优先级

- **P2：UpdateBuf 持久化**：`WeChatManager.Stop()` 落盘 + 启动时 load。阻塞于 iLink 空 buf `GetUpdates` 返回量实测。建议 P0 止血后实测再定优先级。

#### 纵深防御（可选，不阻塞止血）

- 缩小 connGen 检测窗口（`connection.go:756-765` 提前到事件循环前）——减少 c.send 垃圾写入

#### 独立议题

- bgMode 单 bool 竞态（`connection.go:756-765,891-900`）：导致后台结果丢失，非洪泛，单独治理

### 实现顺序

| 阶段 | 内容 | 依赖 |
|------|------|------|
| 1 | P0-1 + P0-2 并行开发 | 无 |
| 2 | 部署验证 P0 效果 | 阶段 1 |
| 3 | 实测 iLink 空 buf 返回量 → 决定 P2 | 阶段 2 |
| 4 | P2 + 纵深 + bgMode 按各自排期 | 阶段 3 |

## 讨论时间线

| Round | 方向 | 主题 | 关键贡献 |
|-------|------|------|---------|
| R1 | Pilot | 盲测：代码现状扫描 | 发现 bgMode 竞态、c.send 跨重连生命周期、connGen 检测窗口、resendPendingResults 机制 |
| R2 | Copilot | 独立分析 + 倾向 | 提出三大主张：handleBackgroundResult 零校验为根因放大器、pollLoop 并发为乱序源、UpdateBuf 不持久化为量来源 |
| R3 | Pilot | 交叉回应 | 认同主张 1/3，部分反驳主张 2（指出 409 丢失被忽略），补充 c.send 第二通路 |
| R4 | Copilot | 反驳 + 收敛 | 证伪第二通路（TypeChatMessage 与 BackgroundResult 代码互斥），确认 iLink 游标语义，简化 P1 为串行队列 |
| R5 | Pilot | 共识草稿 → 质询 | 提交共识草稿，副驾发现两硬伤：P0-2 转后台路径 409 丢失未解决、P0-1 taskID 编码位置错误 |
| R6 | Copilot | 二次质询 | 发现修订版两新硬伤：pending 只存 text 导致 WechatID 串扰、5min TTL 与 resend 机制冲突 |
| R7 | Pilot | 最终定稿 | 接受全部质询，pending 改存完整上下文，resend 加 IsResend 豁免时效，产品决策明确 |

OK @ 2026-07-15 13:15 Pilot R7（最终定稿）
