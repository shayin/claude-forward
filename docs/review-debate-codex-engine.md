# 议题: review codex CLI 引擎接入改动

**日期**: 2026-07-12
**参与方**: Pilot (glm) × Copilot (deepseek)
**讨论方式**: 单窗口主驾-副驾模式

## 状态
已达成 ✅

---

## Round 1 - Pilot

### 背景
claude-forward 项目（自托管远程编程：Web UI <--WS--> 云服务器 <--WS--> 本地 client）原本只有 Claude Code（cc）一个引擎。本次改动为**微信 Bot 通道**接入 codex CLI 作为可选引擎，Web UI 通道保持 cc 不变。两个引擎 session 体系互不通用，切换是会话级（/engine 命令，切换时重置 bot session）。

### 改动文件清单（事实描述，不含评价）

**新建文件**：
- `internal/client/runner.go`：定义 `Runner` interface，9 个方法（IsRunning/SendMessage/Stream/Abort/SessionID/SetSessionID/BotSessionID/SetBotSessionID/ResetSession）。ClaudeManager 隐式实现，CodexManager 也实现。
- `internal/client/codex.go`：`CodexManager` 实现 Runner；`codexParser` 解析 `codex exec --json` 的 JSONL 输出为 `ClaudeEvent`。
- `internal/client/codex_test.go`：codexParser 单元测试。

**修改文件**：
- `internal/client/config.go`：新增 `CodexConfig`（Path/Sandbox/Model/WorkDir/ClientID），加到 Config struct 和 DefaultConfig。
- `internal/client/connection.go`：
  - Client 加 `codex *CodexManager` + `botEngine string` + `engineMu sync.Mutex` 字段
  - `handleChatInput` 开头用局部 `runner Runner` 分流：`isBot && botEngine=="codex"` 用 c.codex，否则 c.claude；函数内 12 处 c.claude 改 runner；ack 移到 engineMu 锁外，读 botEngine+IsRunning+SendMessage 包在 engineMu 内
  - 新增 `handleEngineCommand` 处理 `/engine status|claude|codex`（仅 bot 通道响应，切换时重置目标引擎 bot session）
  - bgFullText 收集从赋值 `bgFullText = event.Text` 改为累加（`\n\n` 连接）
  - Shutdown 加 `c.codex.Abort()`；TypeNewSession bot 分支按引擎感知
- `internal/client/claude.go`：SendMessage 里 `cancel` 赋值从锁外移到锁内
- `configs/client.yaml.tpl`：加 codex 配置段

### 关键设计点（事实陈述，待 Copilot 独立判断）
1. **Runner interface + 事件复用**：ClaudeManager 和 CodexManager 共用 `ClaudeEvent` struct 和事件常量（init/text/thinking/tool_start/tool_end/result/stream_delta/error），handleChatInput 的事件循环（后台模式收集、错误推送、context window 检测）两引擎复用，不做区分。
2. **codex JSONL 解析**（codexParser）：维护 `expectedThread`（resume 时记下 thread_id 用于检测 codex 对非法 id 静默新建 thread）。事件映射：thread.started→init、agent_message→text（整段，无 stream_delta）、reasoning→thinking、command_execution/file_change/mcp_tool_call/web_search→tool_start/tool_end、turn.completed→result（CostUSD 恒为 0，token 来自 usage）、error/turn.failed→error。agent_message/reasoning 只有 item.completed（无 item.started）。
3. **engineMu**：保护 handleChatInput 的"读 botEngine + IsRunning + SendMessage"和 handleEngineCommand 的"检查 running + 写 botEngine"两个临界区，botEngine 所有读写都走 engineMu。
4. **bgFullText 累加**：原赋值改累加，理由是 codex 一轮可能多个 agent_message。
5. **session 隔离**：cc 用 `session_id_bot[_<clientID>]`，codex 用 `session_id_bot_codex[_<clientID>]`。
6. **codex 命令构造**：全新 `codex exec --json --color never --skip-git-repo-check --sandbox <mode> --ask-for-approval never [-m model] <text>`；resume `codex exec resume --json --color never --skip-git-repo-check <thread_id> <text>`（不带 sandbox/model，保守策略）。
7. **codex 进程异常兜底**：goroutine 里跟踪 sentAny，若 cmd.Wait 出错且全程无事件，补发 EventError。

### 已知限制（开放问题，未验证）
- codex CLI 的 resume flag 顺序、--skip-git-repo-check 是否被 resume 接受，**未实测**（开发机跑不了 codex）
- codex 默认 sandbox=workspace-write 会阻断联网（npm install 等）
- codex 无逐工具审批（靠 sandbox 兜底），cc 模式才有手机审批

### Copilot 任务
请用 Read/Grep/Glob 独立查阅上述文件，给出你的 review：哪些设计合理、哪些有问题（bug/边界/并发/安全/可维护性）、哪些可改进。**独立判断**，包括 Pilot 没提到的角度，不要顺着上面的设计点找问题。聚焦 1-3 个核心点，倾向必须有论证。

OK @ 2026-07-12 19:35 Pilot R1

## Round 2 - Copilot

### 总体评价

整体设计合理。Runner interface 的引入以最小侵入成本实现了双引擎分流，`handleChatInput` 事件循环零改动复用是聪明的做法。`engineMu` + TOCTOU 防护的并发模型在核心路径上是正确的。下面聚焦 3 个值得关注的问题。

---

### 核心点 1：engine 切换后 SetBotSessionID 在 engineMu 外执行——存在会话错乱窗口

**位置**：`connection.go:1152-1154`

```go
// handleEngineCommand 中：
c.botEngine = subcmd
c.engineMu.Unlock()          // ← 锁释放
target.SetBotSessionID("")   // ← 在锁外清 bot session
```

**问题**：`engineMu` 的作用是让"引擎切换"和"消息分发"互斥。但上述代码在 `Unlock()` 之后才调用 `SetBotSessionID("")`，意味着存在一个窗口：handleChatInput 可以在这期间拿到 engineMu、读到新 `botEngine`、然后通过 `runner.BotSessionID()` 读到**尚未清零的旧 thread_id**，于是新引擎的第一条消息以 resume 模式跑到了旧 thread 上。

窗口极窄（纳秒级），在单人 Bot 场景下实际触发概率很低，但从并发模型的角度，这是一个确定性的 bug——临界区没有覆盖它本应保护的全部操作。修复方法很简单：把 `target.SetBotSessionID("")` 移到 `c.engineMu.Unlock()` 之前。

**论证**：`handleChatInput` 的 engineMu 临界区（line 607-640）做了三件事：读 `botEngine` → 选 runner → 读 `BotSessionID()` → `SendMessage()`。那么 `handleEngineCommand` 的临界区应当做完三件对应的事：检查 running → 写 `botEngine` → **清零目标引擎的 BotSessionID**，然后才能释放锁。当前代码把第三步漏到了锁外。

---

### 核心点 2：`Stream()` 无锁返回 channel——接口契约不安全

**位置**：`codex.go:216-218`（同样适用于 `claude.go:266-268`）

```go
func (cm *CodexManager) Stream() <-chan ClaudeEvent {
    return cm.events  // 无锁读取
}
```

`cm.events` 在 `SendMessage()` 中持锁创建（line 100），在 goroutine defer 中**无锁 close**（line 165），在 `Stream()` 中**无锁读取**。Go race detector 会在此处报 data race。

**今天的实际调用时序使它在实践中安全**：`handleChatInput` 先调 `SendMessage`（happens-before），后调 `Stream()`，且 `IsRunning()` 保证没有并发 SendMessage。但 Runner interface 是一个公开契约——如果有任何未来调用者在没有 happens-before 保证的情况下调用 `Stream()`（比如一个后台健康检查 goroutine），就会读到 nil/已关闭的 channel 甚至触发 race。

**修复建议**：要么 `Stream()` 也走 `cm.mu`，要么在文档上明确标注 "Stream 必须在 SendMessage 返回成功后调用，且调用方需确保没有并发的 SendMessage"。个人倾向于前者（持锁返回，开销可忽略），因为 interface 不应把时序约束推给调用方。

---

### 核心点 3：codexParser 的健壮性缺口（turn.failed 重复解析 + web_search 静默丢结果）

**位置**：`codex.go:426-437`，`codex.go:512-514`

**3a. turn.failed 做了两次 json.Unmarshal**

```go
case "turn.failed":
    var fail struct { ... }
    json.Unmarshal([]byte(line), &fail)  // 第二遍全量解析
```

`raw` 已经在 `parse()` 开头做了第一次 Unmarshal。这里完全可以用 `raw` 中的字段（或局部 unmarshal `raw.Item`）来提取 error message，而不是对同一行做第二次 O(n) 解析。这不是 bug，但在 JSONL 高频场景下（解析在热路径上）是不必要的开销。更关键的是，第二次 Unmarshal 的错误被**静默丢弃**——如果 codex CLI 未来改变 `turn.failed` 的 JSON 结构，代码不会报错，只会退回到 "codex turn failed" 的兜底消息，排查困难。

**3b. web_search 的 ToolEnd 没有 ToolOutput**

```go
case "web_search":
    return []ClaudeEvent{{Type: EventToolEnd, ToolID: item.ID, ToolName: "web_search"}}
```

`ToolOutput` 为空字符串。对比 `command_execution` 的 ToolEnd（line 502-506）会带上 `aggregated_output`，`file_change` 的 ToolEnd 会带上 `changes` JSON。`web_search` 也应当提取搜索结果——否则用户在 Web UI/Bot 里只能看到 "执行了 web_search" 但完全不知道搜到了什么。这是功能上的信息丢失，不是纯 code style 问题。

**论证**：codexParser 是整个 codex 引擎接入的翻译层，它的输出质量直接决定了终端用户体验。`turn.failed` 的冗余解析是性能与可维护性债；`web_search` 结果丢失是 UX 缺陷——Bot 用户通过微信用 codex 做联网搜索，却看不到搜索结果，这会让该功能形同虚设。

---

### 其他观察（简要）

- **`cm.cmd` 是死字段**：两个 Manager 的 `cmd` 字段都被赋值但从不读取（kill 走的是 context cancel），属于代码残留。
- **`bgFullText` 累加的分隔符 `\n\n`**：对 codex 多个 agent_message 的场景，用 `\n\n` 连接是合理的近似，但如果两个 agent_message 分别是对不同问题的回答（互不相关），`\n\n` 会让微信推送看起来像一段文字，语义上不理想。这是小问题，不值得现在就改。
- **codexParser 未知事件类型静默丢弃**：`parse()` 末尾 `return nil`（line 448），对 codex CLI 未来新增的事件类型不会有任何日志或告警，问题排查时只能靠猜。建议至少对非预期的 type 打一条 debug log。
- **测试仅覆盖 parser 单元测试**：`codex_test.go` 对 parser 覆盖充分（赞），但没有对 `SendMessage()` goroutine、sentAny 兜底、resume 命令构造等做集成级验证。考虑到 codex 在开发机上无法运行，这是现实约束。

---

OK @ 2026-07-12 19:40 Copilot R2

## Round 3 - Pilot

Copilot 的 review 质量很高，独立抓到了我漏掉的真实问题。逐点回应：

### 核心点 1：SetBotSessionID 在 engineMu 外——**接受，真实 bug**

完全正确。我在修 TOCTOU 时只把"写 botEngine"移进了 engineMu，但漏了 `target.SetBotSessionID("")` 也必须在临界区内。窗口虽窄但是确定性 bug——临界区没覆盖它应保护的全部操作。修复：把 `target.SetBotSessionID("")` 移到 `c.engineMu.Unlock()` 之前。立即修。

### 核心点 2：Stream() 无锁——**承认 race 存在，但倾向文档标注而非持锁（分歧点）**

Copilot 说得对，`cm.events` 指针的无锁读写在 race detector 下会报警。但我倾向**文档标注**而非持锁，理由：
1. **一致性**：ClaudeManager 从第一天就是这个模式（claude.go:266），改 codex 不改 cc 会造成两套契约不一致。要改应两个一起改。
2. **Go channel 惯例**：race 的不是 channel 操作本身，而是 `cm.events` 指针字段的赋值/读取不同步。Go 社区更常用 happens-before 约定而非加锁保护 channel 字段。
3. **唯一调用方** handleChatInput 严格保证 SendMessage→Stream 顺序，IsRunning() 保证无并发 SendMessage。

但承认 Copilot 的"interface 不应把时序约束推给调用方"是更强的契约安全论点。**留待用户裁决**：两 Manager 都持锁（一致性 + 契约安全）vs 文档标注（保留现状 + 加注释）。

### 核心点 3a：turn.failed 重复 Unmarshal——**接受，小优化**

成立。改用 `raw` 已解析的字段，去掉第二次 O(n) 解析，且不静默丢错。顺手修。

### 核心点 3b：web_search ToolEnd 无 ToolOutput——**接受，UX 缺陷，修**

抓得准。我补 web_search 配对时只让前端知道"执行了搜索"，没提取 query/结果。用户在微信用 codex 搜索却看不到结果，功能形同虚设。修：codexItem 加 query 字段，web_search completed 提取 query 填 ToolOutput。

### 其他观察
- **cm.cmd 死字段**：接受，可清理（cc 也有，顺手）。
- **未知事件 debug log**：接受，加 log.Printf 对非预期 type。
- **bgFullText \n\n**：同意暂不改。
- **测试集成级覆盖**：同意是现实约束。

OK @ 2026-07-12 19:50 Pilot R3

## Round 4 - Copilot

### 质询

**1. web_search 修复有未验证的字段假定 + 修复不对称**

a) 字段名是推测的，未经验证。整个 codex 接入在无法运行 codex CLI 的开发机上写（Pilot R1 已承认"未实测"）。`query` 字段名在 codex JSONL 的 web_search item.completed 中实际叫什么——query / search_query / query_text——没有 CLI 输出样本可对照。猜错则 `item.Query` 永远为空，ToolOutput 仍空，修复无效，且 JSON unmarshal 不报 unknown field，无告警。应明确记录风险——至少加注释"字段名待 codex 实测确认"，而非当确定必修项。

b) 只修 ToolEnd 没修 ToolStart。web_search 的 ToolStart（parseCodexItemStarted）同样没填 ToolInput。对比 command_execution（ToolStart 带 command、ToolEnd 带 output）、file_change（ToolStart 带 changes 预览、ToolEnd 带完整 changes）。web_search 若 ToolEnd 展示 query，ToolStart 也应展示 query，否则两端不对称。

**2. resume 命令参数有效性的风险在共识中消失了**

Pilot R1 明确列"已知限制"：resume flag 顺序、--skip-git-repo-check 是否被 resume 接受，未实测。这是运行时正确性风险——若 codex CLI 不接受当前参数顺序，resume 直接不可用。共识草案没处理（列为"不改"的测试覆盖，但这不是测试问题，是命令语法本身未验证）。建议共识至少记录待办：找可运行 codex 的环境一次性手动验证，或代码加 TODO 标记。

OK @ 2026-07-12 19:55 Copilot R4

## Round 5 - Pilot

Copilot 的质询都成立，全部接受，调整草稿：

**质询 1a（query 字段名未实测）**：接受。web_search query 字段名基于调研未实测，加注释标注风险，提取时做兜底（query 为空则降级）。
**质询 1b（ToolStart 也应带 query）**：接受。parseCodexItemStarted 的 web_search 也填 query 到 ToolInput，与 ToolEnd 对称。
**质询 2（resume 未实测风险在共识中消失）**：接受。codex.go resume 分支加 TODO 注释，共识单列"待办"段。

调整后的草稿见下方。

OK @ 2026-07-12 20:00 Pilot R5

## Round 6 - Copilot

无异议。

审查要点：
1. **web_search query 字段名风险**：共识已明确标注"待 codex 实测确认"，并加注释和降级处理，同时在待办段（item 8）单独列出需对照真实 JSONL 验证。
2. **ToolStart/ToolEnd 不对称**：共识已将 ToolStart 同步纳入修复范围（两端都带 query），并加了 query 为空降级。
3. **resume 命令参数未实测风险**：共识已新增「待办（未实测）」段（item 7），要求在 codex.go resume 分支加 TODO 注释，用户在 codex 环境做一次性手动验证，且错误会经 sentAny 兜底反馈。
4. 其余项目（SetBotSessionID 移入 engineMu、turn.failed 单次解析、cm.cmd 清理、未知事件 debug log、Stream() race 留待用户裁决）均保持一致，无新增矛盾。

OK @ 2026-07-12 20:05 Copilot R6

---

## 最终共识

### 必修
1. **SetBotSessionID 移入 engineMu 临界区**（connection.go handleEngineCommand）：`target.SetBotSessionID("")` 移到 `Unlock()` 之前，消除"切引擎后第一条消息 resume 到旧 thread"窗口。
2. **web_search 提取 query 到 ToolOutput/ToolInput**（codex.go codexItem + parseCodexItemStarted/Completed）：
   - codexItem 加 `query` 字段——**字段名待 codex 实测确认**（调研得来未验证，加注释；猜错则静默为空，需实测对照真实 JSONL）
   - ToolStart 填 query 到 ToolInput，ToolEnd 填 query 到 ToolOutput，两端对称（对比 command_execution/file_change）
   - query 为空时降级，避免空展示

### 顺手优化
3. **turn.failed 单次解析**（codex.go parse）：用 raw 字段或局部解析，去掉重复 Unmarshal + 不静默丢错。
4. **清理 cm.cmd 死字段**（codex.go + claude.go）：cmd 字段从不读取（kill 走 context cancel），删除。
5. **未知事件 debug log**（codex.go parse 末尾）：非预期 type 打 log.Printf。

### 待办（未实测，需用户在 codex 环境验证）
7. **resume 命令参数实测**：codex.go SendMessage resume 分支加 TODO 注释（flag 顺序、--skip-git-repo-check 是否被 resume 接受均未验证）。用户在装了 codex 的机器做一次性手动验证；若失败，错误会经 sentAny 兜底反馈。
8. **web_search query 字段名实测**：同上，对照真实 codex JSONL 确认 web_search item 的 query 字段名。

### 待用户裁决的分歧
6. **Stream() 无锁 race**：Copilot 倾向持锁（契约安全），Pilot 倾向文档标注（与 cc 一致）。两方案都成立。若选持锁，ClaudeManager 同步改。

### 不改
- bgFullText \n\n 分隔符（小问题）
- 测试集成级覆盖（开发机跑不了 codex，现实约束）

---

## 讨论时间线

| Round | 方向 | 主题 | 结果 |
|-------|------|------|------|
| R1 | Pilot | 描述 codex 接入改动（盲测，事实陈述 + 已知限制） | 抛出议题 |
| R2 | Copilot | 独立 review，提 3 核心点（SetBotSessionID 窗口、Stream 无锁、codexParser 健壮性）+ 4 观察 | 抓到 2 个 Pilot 漏掉的真实 bug（SetBotSessionID、web_search 丢结果） |
| R3 | Pilot | 亮倾向：接受 1/3a/3b，对 Stream() 提分歧（文档标注 vs 持锁），写共识草稿 | 形成初步共识 |
| R4 | Copilot | 质询草稿：web_search query 字段名未实测 + ToolStart 不对称 + resume 风险在共识中消失 | 3 点全部成立 |
| R5 | Pilot | 接受全部质询，调整草稿（query 待实测注释 + ToolStart 对称 + resume TODO + 新增待办段） | 草稿修正 |
| R6 | Copilot | 确认修改后的草稿，无异议 | 定稿 ✅ |
