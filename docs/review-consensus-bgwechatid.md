# 断线自动后台 wechatID 修复 — GLM × DS 讨论共识

**日期**: 2026-06-14
**参与方**: GLM (`docs/review-glm-bgwechatid.md`) × DS (`docs/review-ds-bgwechatid.md`)
**讨论方式**: 双向监听对方文件，一问一答
**实现归属**: A（GLM 窗口）实施

## 状态
**最终共识已达成 ✅**（R1-R3）

---

## 一、议题

断线重连触发自动后台时，`bgWechatID` = `TrimPrefix(userID, "bot-")` = `"wechat-{uuid}"`（无效），后台结果推送 100% 失败，用户收不到结果。

**根因**：Client 端根本不知道真实 wechatID。真实 wechatID 只在 Server 端 `chatViaHub` 的函数参数里（`wechat.go:378`），从未传给 Client。断线自动后台发生在 Server 发 `TypeBackgroundMode` 之前，所以 Client 无从获取正确值。

**损坏代码**：`connection.go:713` `c.bgWechatID = strings.TrimPrefix(userID, "bot-")`

---

## 二、方案对比

| 方案 | 描述 | 结论 |
|------|------|------|
| **A（选定）** | 改协议，`ChatInputPayload` 加 `WechatID` 字段，Server 发消息时携带 | ✅ 最直接，Client 持有正确数据 |
| B | 自动后台时 wechatID 留空，Server 端反查 | ❌ 反查逻辑 + Hub.Unregister 清理导致不可靠 |
| C | Server 把 wechatID 存到 Connection 结构体 | ❌ 污染通用抽象 + botConn 生命周期问题 |
| D | 利用已有 `BackgroundModePayload.WechatID` 延迟补值 | ❌ 依赖异步时序，回退路径同 B |

**为什么用 `TypeChatInput` 而非 `TypeAttach`**：
- `ChatInputPayload` 已有结构体（`message.go:131`），加字段是最小改动
- `TypeAttach` 虽然 `AttachPayload` 类型存在（`message.go:79`），但 Server 发送时没用 payload（`wechat.go:415-418`）
- `TypeChatInput` 语义更清晰：每条消息携带当前微信用户，多用户场景互不干扰

---

## 三、最终共识

### 3.1 实施方案

**Server 端**（`protocol/message.go` + `server/wechat.go`）：

1. `ChatInputPayload` 加字段：
   ```go
   type ChatInputPayload struct {
       Text     string `json:"text"`
       WechatID string `json:"wechat_id,omitempty"`
   }
   ```

2. Server `chatViaHub` 发 TypeChatInput 时填 WechatID：
   ```go
   chatMsg, err := protocol.NewMessage(protocol.TypeChatInput, protocol.ChatInputPayload{
       Text:     text,
       WechatID: wechatID,  // wechatID 已是 chatViaHub 的参数（wechat.go:378）
   })
   ```

**Client 端**（`client/connection.go`）：

3. Connection 结构体加字段（与 bgMode/bgTaskID/bgWechatID 同组）：
   ```go
   currentWechatID string  // 当前正在处理的微信用户 ID（来自 ChatInputPayload.WechatID）
   ```
   - **用 `bgMu` 保护**（复用现有锁，不新增锁）

4. `handleChatInput` 解析 payload 时更新字段：
   ```go
   c.bgMu.Lock()
   c.currentWechatID = payload.WechatID
   c.bgMu.Unlock()
   ```

5. 修复 line 713：
   ```go
   // 旧：c.bgWechatID = strings.TrimPrefix(userID, "bot-")
   c.bgWechatID = c.currentWechatID
   ```

### 3.2 关键决策记录

- **字段而非局部变量**：connGen 检测在事件循环深处（`connection.go:710-717`），与 `handleChatInput` 入口处的 payload 解析不在同一作用域，局部变量传不下去。必须用 Connection 字段。
- **复用 bgMu 锁**：与 bg 状态字段同组，避免新锁；写入在 handleChatInput 解析 payload 时，读取在 connGen 检测的 `c.bgMu.Lock()` 块内，天然安全。
- **版本不匹配回退非新 bug**：老版本 Server 不发 WechatID 时 `currentWechatID` 为空，line 713 回退到空字符串，与当前错误行为一致（推送失败）。不引入额外兼容逻辑。Server + Client 同时部署，不会出现版本不匹配。

### 3.3 改动量

3 个文件，约 10-15 行代码。

### 3.4 实施中发现：第二个 bug 位置（共识外补充）

实施时发现 `connection.go` 还有**第二处**相同的 TrimPrefix 错误：

```go
// line 779（原）：非后台模式下的错误通知推送
wechatID := strings.TrimPrefix(userID, "bot-")  // 同样得到无效的 "wechat-{uuid}"
```

**讨论时（R1-R3）我和 DS 都漏看了这处。** 它不在自动后台路径，而是"非后台模式收到 Claude 错误事件时主动推送错误通知"的路径。同一个根因（Client 不知道真实 wechatID），同一个修复（改用 `c.currentWechatID`）。

已一并修复，无需重新讨论。

---

## 四、讨论时间线

| Round | 方向 | 主题 | 结果 |
|-------|------|------|------|
| R1 | GLM → DS | 根因定位 + 方案 A/B/C + 倾向 A | 抛选择 |
| R1 | DS → GLM | 同意 A，明确 TypeChatInput 载体，补方案 D 排除 | A + TypeChatInput 定向 |
| R2 | GLM → DS | 事实核对通过，反驳"局部变量"，提议 currentWechatID 字段 + bgMu | 字段方案 |
| R2 | DS → GLM | 接受字段方案，补版本不匹配回退说明 | 共识达成 |
| R3 | GLM → DS | 收尾确认 | 讨论结束 |
