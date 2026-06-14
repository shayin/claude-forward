# 超时转后台 Bug — GLM × DS 讨论共识

**日期**: 2026-06-14
**参与方**: GLM (`docs/review-glm-timeout-bg.md`) × DS (`docs/bug-review-ds-2026-06-14.md`)
**讨论方式**: 双向监听对方文件，一问一答

---

## 状态

**最终共识已达成 ✅**（R1-R7，双方确认）。讨论结束。

---

## 一、双方报告对照矩阵

| 维度 | GLM 位置 | DS 位置 | 是否一致 | 备注 |
|------|---------|---------|----------|------|
| tool 事件不 reset 超时 | 问题 1 (High) | 问题 6 (中等) | ✅ 都识别到 | 讨论后统一为 P0 |
| 断线重连 bgWechatID 错误 | 问题 2 (High) | 问题 5 (中等) | ✅ 都识别到 | 讨论后统一为 P1 |
| 多 botConn 事件路由（只发第一个） | 问题 4 (Medium) | 问题 2 (严重) | ✅ 都识别到 | 讨论后统一为 P2 |
| 10 秒延迟清理窗口 | 问题 3 (Low) | 问题 3 (严重) | ✅ 都识别到 | 归入 Hub.Unregister 修复 |
| botConn.Send 不关闭 | 问题 7 (Low) | 问题 7 (轻微) | ✅ 共识 | P3 代码质量 |
| 超时后仍发 TypeDetach | （未单独列出） | 问题 4 (中等) | ⚠️ 仅 DS 单独列出 | 待评估是否单独修 |
| Hub.Unregister 清空 attachMap | （并入问题 4） | 问题 1 (严重·核心根因) | ⚠️ 角度差异 | 讨论后定为 P1（非核心根因） |
| hardTimeout 与自动后台竞态 | 问题 6 (Low) | — | ⚠️ 仅 GLM | P3 边缘情况 |
| bgMode 10秒轮询不够 | GLM 问题 5 (Medium) | DS 问题 8 (轻微) | ⚠️ 评级不一致 | 讨论后统一为 P2，建议重新设计而非拉长 |

---

## 二、关键分歧的解决

### 分歧 1：核心根因到底是谁？
- **DS 原主张**：Hub.Unregister 在 Client 断线重连时清空 attachMap，是用户报告"每句话都发超时"的真凶
- **GLM 原主张**：timer 不对 tool 事件 reset，是"每句话都发超时"的真凶
- **解决（R1-R4）**：DS 接受 GLM 的两个反问——
  - Hub.Unregister 路径无法解释"实时返回"（如果走超时路径，回复只能是后台一次性推送，不是流式）
  - Hub.Unregister 要"每句话都触发"需要 WebSocket 每 30 秒断一次，过于极端
- **结论**：timer 不 reset 是稳定根因（P0），Hub.Unregister 仅在断线场景附加触发（P1）

### 分歧 2：bgWechatID 的严重等级
- **DS 原评级**：中等
- **GLM 反驳**：这是**确定性 100% 触发**的 bug——只要 connGen 变化（断线重连），`TrimPrefix("bot-wechat-{uuid}", "bot-")` 永远得到无效 wechatID
- **解决**：DS 同意，bgWechatID 提为 P1

### 分歧 3：timer 修复策略（A vs B）
- **选项 A**：收到任何事件都 reset
- **选项 B**：保持仅文字 reset，把 timeout 拉到 10 分钟
- **结论（R3-R4）**：选 A。Server timeout 退化为纯心跳检测；真正的卡死由 Client noOutputTimer(30min) 兜底；hardTimeout(30min) 做绝对上限。B 方案劣化真正卡死的检测延迟。

---

## 三、最终共识（双方确认 ✅）

### 3.1 优先级与触发条件

| 等级 | 问题 | 触发条件 | 影响 | 触发性质 |
|------|------|----------|------|----------|
| **P0** | timer 不 reset（tool_start/tool_end/thinking） | 每次工具调用 >3min 无文字输出 | 每句话都误报超时 | **稳定必现** |
| **P1** | bgWechatID 错误赋值 | 断线重连触发自动后台 | 后台结果 100% 推送失败 | **确定性必现** |
| **P1** | Hub.Unregister 清空 attachMap | 断线重连 | chatViaHub 失去事件路由，附加误报 | 断线时触发 |
| **P2** | 并发消息 attachedUser 覆盖 | 前任务运行中发新消息 | 事件路由混乱 | 取决于用户使用习惯 |
| **P2** | bgMode 轮询 10s 不够 | 弱网/macOS 睡眠 | 后台结果丢失 | 网络场景相关 |
| **P3** | botConn.Send 不关闭 | 一直存在 | 潜在资源泄漏 | 代码质量 |
| **P3** | hardTimeout 与自动后台竞态 | 边缘时序 | 极少触发 | 边缘情况 |

### 3.2 修复策略

**P0 修复方向**：
- 在 `wechat.go:463-484` 的事件 switch 中，给 `tool_start` / `tool_end` / `thinking` 三个 case 加 `timeout.Reset(3 * time.Minute)`
- `result` case 不需要 reset（马上 return）
- `TypeChatAck` 不 reset（仅 WS 传输确认，不代表 Claude 在工作）
- **代码注释必须更新设计意图**：commit `92caba1` 当时假设"工具调用都在 3 分钟内"已过时，要写明"任何 Claude 事件都视为活跃信号"

**P1 bgWechatID 修复方向**：
- 自动后台分支（`connection.go:701-707`）不应从 `botConn.ID` 反向解析 wechatID
- 正确做法：参数传入实际微信用户 ID；或从已收到的 `TypeBackgroundMode` 消息中读取 `bgWechatID`（如果已设置）
- 注意 `botConn.ID = "bot-wechat-" + uuid`，uuid 是随机的，根本不含微信用户 ID 信息

**P1 Hub.Unregister 修复方向**：
- `Hub.Unregister` 清理 `attachMap` 时，跳过 bot 连接（ID 以 "bot-" 开头的条目）
- 或：`handleChatClientToUser` 改为向所有匹配的 user 广播，而不是只发给第一个
- **P1-3 子方案（DS 补充，R6）**：将 `chatViaHub` 的 botConn 与 Client 连接 ID 做显式绑定，不依赖 attachMap 的隐式映射。这样即使 Client 重连后，事件也能正确路由到对应 botConn。

**P2 bgMode 轮询修复方向**：
- 不要简单拉长轮询时间，应改成基于 channel/signaling 的同步等待
- 用 `context.Done()` 或专用 channel 通知 `bgMode` 设置完成

### 3.3 建议修复顺序

| 顺序 | 问题 | 理由 |
|------|------|------|
| 1 | P0 timer reset | 修完后绝大多数任务不再误转后台，断线场景才暴露 P1。用户立即感知改善。 |
| 2 | P1 bgWechatID | 独立 bug，确定性必现，修复简单。 |
| 3 | P1 Hub.Unregister | 涉及 Hub 核心数据结构，风险略高，建议后修。 |
| 4 | P2 系列 | 修完 P0/P1 后再评估实际影响。 |

---

## 四、移交给用户的开放问题

以下事实需要用户确认，决定最终修复的细节：

1. **「实时返回」具体指什么**？
   - 如果是流式文字一段段出现在微信里 → 印证 P0 是核心根因（chatViaHub 正常返回 + 后台误报）
   - 如果是任务完成后突然收到一大段 → 印证 P1 路径为主（Hub.Unregister + 后台结果推送）

2. **用户 macOS Client 的断线频率**？
   - 经常断线（macOS 睡眠、网络切换）→ P1 影响大
   - 网络稳定 → P1 影响小，P0 是绝对主因

3. **典型 Claude Code 任务时长**？
   - 是否经常 >3 分钟无文字输出（长 Bash、Agent 子任务）？
   - 决定 timer reset 策略是否充分

4. **用户使用模式**？
   - 是否经常在前任务运行时发新消息？
   - 决定 P2「并发消息 attachedUser 覆盖」的实际影响

---

## 五、讨论时间线

| Round | 方向 | 主题 | 结果 |
|-------|------|------|------|
| R1 | GLM → DS | 质疑 Hub.Unregister 是核心根因 | 抛出两个反问 |
| R2 | DS → GLM | 同意 timer 为 P0，Hub.Unregister 降 P1 | 共识 #1 达成 |
| R3 | GLM → DS | 选 A 策略，bgWechatID 提 P1 | 共识 #2/#3 待确认 |
| R4 | DS → GLM | 三点全部同意 | 共识 #2/#3 达成 |
| R5 | GLM → DS | 补充修复顺序、设计意图更新、共识稿策略 | 等 R6 确认 |
| R6 | DS → GLM | 全部同意，补充 P1-3 子方案（botConn 显式绑定 Client 连接 ID） | 共识达成 |
| R7 | DS → GLM | 审阅共识稿，提两处小修正，确认通过 | 讨论结束 ✅ |
