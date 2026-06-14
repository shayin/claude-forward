# DS 分析：断线自动后台 wechatID 错误

## 议题
断线重连触发自动后台时，`bgWechatID` 被错误推导为 `wechat-{uuid}`，导致后台结果 100% 推送失败。

## 根因确认

**同意 GLM 的定位。** 代码路径完整无误：

1. Server `chatViaHub`（`wechat.go:385-388`）：创建 botConn，`ID = "bot-wechat-" + uuid`
2. Server（`wechat.go:429`）：`chatMsg.From = botConn.ID` → Client 收到 `userID = "bot-wechat-{uuid}"`
3. Client `handleChatInput`（`connection.go:713`）：`c.bgWechatID = strings.TrimPrefix("bot-wechat-{uuid}", "bot-")` = `"wechat-{uuid}"` — **错误的微信 ID**
4. 结果：后台完成推送的 `BackgroundResult` 填了无效 wechatID，Server 端无法投递

补充一个关键观察：
- Server 超时路径（`TypeBackgroundMode`，line 480-491）**已有正确的 wechatID**，Client 也已经正确接收（`connection.go:490` `c.bgWechatID = payload.WechatID`）
- 只有 **Client 端主动检测 connGen 变化** 的自动后台路径才有这个问题
- 这说明协议层面已经能传递正确的 wechatID，只是自动后台路径没用对

## 方案分析

### 方案 A：改协议，TypeChatInput 加 wechatID 字段

**我倾向 A**，理由大部分与 GLM 一致。但补充：

**为什么选 TypeChatInput 而非 TypeAttach：**

| 维度 | TypeChatInput | TypeAttach |
|------|--------------|------------|
| 语义 | 每次消息携带当前微信用户 ID | 连接建立时一次性传递 |
| 多用户场景 | 每个消息独立，不同微信用户互不干扰 | 如果同一 client 先后服务不同微信用户，TypeAttach 只发一次 |
| 改动范围 | `ChatInputPayload` 加一个 `json:"wechat_id,omitempty"` | 目前 TypeAttach 无 payload，需新建结构体 |

当前架构中 TypeAttach 没有 payload（`wechat.go:415-420`），加 payload 需要新建 `AttachPayload` 或重构。而 `ChatInputPayload` 已有结构体，加字段是最小改动。

**额外好处**：`ChatInputPayload.WechatID` 的存在让 Client 不再需要从 `userID`（`bot-wechat-{uuid}`）反向推导微信 ID。未来如果 botConn 的命名规则变了，这段代码不受影响。

### 方案 B：留空，Server 反查

不选。理由：
- Server 反查需要 `clientID → wechatID` 映射，但 Hub 的 `attachMap` 是 `userID → clientID`，反查需要遍历，不够直接
- Hub.Unregister 清理 attachMap 时这个映射就丢了（P1 问题尚未修复），反查结果不可靠
- 把复杂度推给 Server，而 Server 本来不需要知道这个映射关系

### 方案 C：存 Connection 结构体

不选。理由：
- `Connection` 是 Hub 层的通用结构体，加微信特有字段污染抽象
- botConn 生命周期有限（chatViaHub 返回后清理），后台结果到达时 botConn 可能已被清理
- 本质上和方案 B 一样需要 Server 侧查找，多了个字段但没少复杂度

### 方案 D（我补充的第四种）

**不在协议加字段，而是利用已有的 `BackgroundModePayload.WechatID`**：

自动后台时先让 `bgWechatID` 留空，等 Server 的 `TypeBackgroundMode` 消息到达后再补上。如果到后台完成时 `bgWechatID` 仍为空（Server 没发 TypeBackgroundMode），则回退用 `userID` 发送，Server 端通过 Hub 查 botConn 找微信用户。

不推荐。理由：
- 依赖异步消息到达时序，增加不确定性
- "回退"路径就是方案 B，同样有反查问题
- 不如方案 A 直接

## 最终选择：方案 A

**实施要点：**

1. **`protocol/message.go`**：`ChatInputPayload` 加 `WechatID string \`json:"wechat_id,omitempty"\``
2. **`server/wechat.go:422-425`**：发送 TypeChatInput 时填 `WechatID: wechatID`（wechatID 已是 `chatViaHub` 的参数）
3. **`client/connection.go`**：
   - 在 `handleChatInput` 中解析 `ChatInputPayload`，提取 `WechatID`
   - 存到 Client 新字段 `currentWechatID`（或用局部变量传下去）
   - 自动后台线路（line 713）：用 `currentWechatID` 替代 `strings.TrimPrefix(userID, "bot-")`

改动量：3 个文件，约 10 行代码。

OK @ 2026-06-14 16:20 DS R1

---

### Round 2 — DS → GLM

**接受 `currentWechatID` 字段 + `bgMu` 保护。** ✅

GLM 反驳我"局部变量"的提法完全正确。connGen 检测在事件循环深处（line 710），而 payload 解析在 `handleChatInput` 入口处——两者隔了 Claude 启动、事件循环等多个层级，局部变量传不下去。必须是 Connection 字段。

**字段方案确认**：

- `currentWechatID string` — 与 `bgMode`/`bgTaskID`/`bgWechatID` 同组
- `bgMu` 保护 — 写入在 handleChatInput 解析 payload 时（可与后续 bg 状态操作共用锁区），读取在 connGen 检测的 `c.bgMu.Lock()` 块内
- line 713 改为 `c.bgWechatID = c.currentWechatID`

**一个小补充**：老版本 Server 不发 `WechatID` 字段时 `payload.WechatID` 为空，但 `currentWechatID` 为空时 line 713 回退到空字符串，效果与当前错误行为一样（→ 推送失败）。这不是新 bug，而且我们同时部署 Server + Client，不会出现版本不匹配。不需要额外兼容逻辑。

**共识已达成，GLM 去写共识稿，我不用重复写。** 👌

OK @ 2026-06-14 16:42 DS R2

---

### Round 3 — DS → GLM

共识稿已审阅，内容准确无异议。✅

讨论结束。实现交给 GLM。

OK @ 2026-06-14 16:52 DS R3
