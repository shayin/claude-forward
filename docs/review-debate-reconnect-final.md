# Cross 模式最终共识：服务端重启后微信消息洪泛 + 顺序错乱

**日期**: 2026-07-15
**Direction 1**: glm 主驾 × deepseek 副驾（8 轮，含质询）
**Direction 2**: deepseek 主驾 × glm 副驾（7 轮，含两轮质询）
**状态**: 已合并 ✅（待用户裁决修复范围）

---

## 双方共识（高可信，核心根因一致）

### 根因 1：BackgroundResult 是唯一能穿透服务端重启的消息类型（洪泛主因）

两个 direction 从不同侧锁定同一条主链：
- **d1（client 侧）**：`c.send` 是 client 级单例 buffered channel（cap=256），断线时 writePump 退出但 `handleChatInput` goroutine 独立存活继续写入 → BackgroundResult 堆积在内存 channel → 重连后新 writePump 全 flush。且 30s 超时落盘分支**典型场景下不可达**（buffered channel 非阻塞写入，单任务产 1 条 ≪ 256）→ 几乎全堆积、不落盘。
- **d2（server 侧）**：`handleBackgroundResult`（handler.go:421）零校验直推 `PushMessage(payload.WechatID)`，**绕过 attachMap、无幂等去重、无时效校验**。普通 ChatMessage/ChatReady 因依赖 attachMap（重启后清空）被静默丢弃，**只有 BackgroundResult 能穿透** → 这正是"选择性疯狂重发"的原因。

**合并结论**：d1 治源头（不让断线结果进 c.send），d2 治出口（server 推送前校验），**两者互补，都应做（纵深防御）**。

### 根因 2：pollLoop 并发导致顺序错乱 + 消息丢失

- d2 定位为 P0-2：`wechat.go:258` `go m.handleMessage` 批量并发，多 chatViaHub 抢写 client.Send，顺序 ≠ 用户发送序；前一条未完成时后续撞 client `IsRunning()` 409 **丢失**。
- d1 列为"独立议题"（顺序来源 B），未展开修法。
- **合并**：采用 d2 的修法方向（per-wechatID 串行队列 + client pending 队列），但优先级可降（见分歧）。

### 共识修复点（双方都提）
- `savePendingResult` 当前单文件覆盖式 → 改按 taskID 多文件，避免多条断线结果互相覆盖
- server `handleBackgroundResult` 需按 taskID 幂等去重

---

## 修复策略（合并后，按优先级）

### P0 止血（立即做，互不依赖可并行）

**P0-a：client 源头落盘（d1 主修复）**
- 位置：`connection.go:912-925`
- 改动：select 之前加 startGen 判定——`if connGen != startGen { savePendingResult(payload) } else { c.send <- resultMsg }`
- 复用 `:675` 已记录的 startGen，改动极小、无需新字段
- 效果：断线期间的 BackgroundResult 根本不进 c.send，重连后只走 resendPendingResults 单一路径

**P0-b：server 推送闸门（d2 P0-1）**
- 位置：`handler.go:397-427` + `connection.go` 所有 BackgroundResultPayload 构造点
- 协议：`BackgroundResultPayload` 新增 `CreatedAt int64`（UnixMilli）、`IsResend bool`
- Client：所有构造点设 CreatedAt；resendPendingResults 读盘时设 IsResend=true（保留原 CreatedAt）
- Server 校验：非 resend 拒绝 CreatedAt >5min；所有类型 taskID LRU 去重（容量 100）；resend 豁免时效（保留去重）
- 效果：即使 P0-a 有漏网之鱼，server 兜底拦截

**P0-c：savePendingResult 多文件按 taskID（双方共识）**
- `pending_results/{clientID}/{taskID}.json`，resendPendingResults 遍历 → 逐个处理 → 成功后删除

### P1 顺序 + 丢失（止血后做）

**P1-a：pollLoop per-wechatID 串行队列（d2 P0-2 server 侧）**
- `wechat.go:258-260`，wechatUserState 加 msgQueue，processLoop 串行消费
- 根治顺序错乱

**P1-b：client pending 队列（d2 P0-2 client 侧）**
- `connection.go:623-631` 409 分支改 pending 排队（buffer 8）
- **pending 项必须存完整 `{Text, WechatID, From}`**（d2 质询抓的硬伤：只存 text 会导致 currentWechatID 串扰 → 推错微信用户）
- 递归上限 5 层
- 缓解转后台场景的 409 丢失（d2 质询修正：转后台路径 chatViaHub return ≠ client 任务结束，409 仍触发）

**P1-c：延迟重放 + ⏮ 标记（d1，顺序来源 A 的体验）**
- resendPendingResults 改 mark（存内存 pending，不立即推）+ **mark 时立即删盘** + **pending 按 taskID 去重**（d1 质询抓的硬伤：多次 Connect 累积重复）
- 新任务结果送达后 flush pending，旧结果加 `⏮ [断线前任务结果]` 前缀
- 超时兜底（重连后 N 分钟无新结果也 flush）

### P2 / 独立议题（按各自排期）

- **UpdateBuf 持久化**（d2 单边）：`WeChatManager.Stop()` 落盘 + 启动 load。**阻塞于实测**：iLink 空 buf GetUpdates 返回量（全部历史 vs 近期未读）——决定优先级
- **bgMode 单 bool 竞态**（d2 单边，`connection.go:756-765,891-900`）：多并发 handleChatInput 时第二个任务结果丢失（非洪泛，独立 bug）
- **:878-890 10s 轮询超时无兜底**（d1 单边 follow-up）：server TypeBackgroundMode 丢失时 bgActive 不触发 → BackgroundResult 不产生 → 任务结果静默丢失
- **drain 防御**（d1 P2）：Connect() 开头、新 writePump 启动**之前** drain 一次 c.send，清理失效残留（belt-and-suspenders）
- **in-flight 丢失**（d1）：writePump 取出消息→WriteMessage 失败→消息已离 channel 不可恢复，接受少量丢失

---

## 双方分歧（需用户裁决）

### 分歧 1：主修复切入点（但其实互补，建议都做）
- **d1**：client 端 startGen 判定（源头不让断线结果进 c.send）
- **d2**：server 端 handleBackgroundResult 闸门（出口校验）
- **裁决建议**：**两者不冲突，是纵深防御的两层**。P0-a（client 源头）+ P0-b（server 闸门）都做。d2 质询已证明单靠 server 闸门不够（taskID 多生成点格式不一），d1 的 client 源头 + d2 的 server 独立字段（CreatedAt/IsResend）组合最稳。

### 分歧 2：pollLoop 串行化（顺序 B）的优先级
- **d1**：列为独立议题，本次只治洪泛不治顺序
- **d2**：列为 P0-2（与洪泛同级），认为同时解决顺序 + 丢失
- **裁决建议**：**先 P0 止血洪泛（P0-a/b/c），再 P1 治顺序（P1-a/b）**。理由：洪泛是用户报告的主痛点（"疯狂推"），顺序错乱是次要；且 P1 改动大（串行队列 + pending），先验证 P0 效果再做 P1 风险更低。d2 质询也承认 P0-2 转后台路径 409 丢失未完全解决，不宜列为同 P0 级。

---

## 单边发现汇总（需用户裁决是否采纳）

### d1 单边（glm 主驾发现）
- **startGen 判定**（已纳入 P0-a）
- 30s 落盘"不可达代码"定性（诊断洞察，非修复）
- 延迟重放 + ⏮ 标记（已纳入 P1-c）
- :878-890 轮询超时兜底（P2 follow-up）

### d2 单边（deepseek 主驾发现）
- **handleBackgroundResult CreatedAt/IsResend 闸门**（已纳入 P0-b）
- **pollLoop 串行队列 + client pending**（已纳入 P1-a/b）
- **UpdateBuf 不持久化**（P2，待实测）—— d1 完全没覆盖这个洪泛"量"的来源
- bgMode 单 bool 竞态（独立 bug）

---

## 建议的实施顺序

| 阶段 | 内容 | 依赖 |
|------|------|------|
| 1 | P0-a（client startGen 落盘）+ P0-b（server 闸门）+ P0-c（多文件）并行 | 无 |
| 2 | 部署验证 P0 效果（kill server → 重连 → 看是否还洪泛） | 阶段 1 |
| 3 | P1-a/b（串行队列 + pending）治顺序+丢失 | 阶段 2 |
| 4 | P1-c（延迟重放+标记）| 阶段 3 |
| 5 | 实测 iLink 空 buf 返回量 → 决定 P2（UpdateBuf 持久化）优先级 | 阶段 2 |
| 6 | 其余独立议题（bgMode 竞态、轮询兜底、drain）按排期 | — |

## 两份原始讨论文件
- Direction 1（glm × deepseek）: docs/review-debate-reconnect-d1.md
- Direction 2（deepseek × glm）: docs/review-debate-reconnect-d2.md
