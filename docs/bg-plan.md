# /bg 后台子任务 — 待实现计划

## 目标

支持微信用户通过 `/bg xxx` 命令主动将任务放到后台执行，自己可以继续做别的事。后台任务完成后自动推送结果。

## 现状

已完成"超时转后台"功能：
- 协议层：`TypeBackgroundStart`、`BackgroundStartPayload` 已定义
- 客户端：`ClaudeRunner` 独立运行能力待实现（当前只有主 ClaudeManager）
- 服务端：`handleBackgroundResult` 已实现推送逻辑

## 调用流程

```
微信用户 ──"/bg 重构代码"──► handleCommand()
                                     │
                               [新增] 检测 /bg 前缀
                                     │
                              TypeBackgroundStart
                              {task_id, text, wechat_id}
                                     │
                                     ▼
                          Client.handleBackgroundStart() [新增]
                                     │
                            创建独立 ClaudeRunner
                            不阻塞主 ClaudeManager
                                     │
                              Claude 完成 → TypeBackgroundResult
                                     │
                              Server → PushMessage() 推送
```

## 执行步骤

### 步骤 1：客户端 — 独立 ClaudeRunner

涉及文件：`internal/client/claude.go`

新增 `ClaudeRunner` 结构体，封装单次 Claude 执行：

```go
type ClaudeRunner struct {
    config  ClaudeConfig
    cmd     *exec.Cmd
    cancel  context.CancelFunc
    events  chan ClaudeEvent
    done    chan struct{}
}

func NewClaudeRunner(config ClaudeConfig) *ClaudeRunner
func (r *ClaudeRunner) Start(text, resumeSessionID string) error
func (r *ClaudeRunner) Stream() <-chan ClaudeEvent
func (r *ClaudeRunner) Wait()
func (r *ClaudeRunner) Abort()
```

`ClaudeManager` 内部改用 `ClaudeRunner` 执行。后台任务直接创建独立的 `ClaudeRunner`。

### 步骤 2：客户端 — /bg 命令处理

涉及文件：`internal/client/connection.go`

新增 `handleBackgroundStart` 方法：
- 创建独立的 `ClaudeRunner`（不复用主 session）
- 启动新的 `claude -p` 进程
- 收集结果后发送 `TypeBackgroundResult`

Connection 新增：
```go
backgroundTasks map[string]*backgroundTask  // taskID → task
bgTaskMu       sync.Mutex
```

需要限制最大后台任务数（建议 3 个），防止资源耗尽。

### 步骤 3：服务端 — /bg 命令

涉及文件：`internal/server/wechat.go`

在 `handleCommand` 中新增 `/bg` 命令：
1. 提取任务描述
2. 生成 taskID
3. 发送 `TypeBackgroundStart` 到 Client
4. 回复用户"后台任务已启动"

### 步骤 4：服务端 — TaskID 校验

涉及文件：`internal/server/handler.go`

在 `chatViaHub` 发送 `BackgroundMode`/`BackgroundStart` 时记录 taskID → wechatID 映射。
`handleBackgroundResult` 校验 taskID 存在后才推送，防止 Client 伪造。

### 步骤 5：测试

- ClaudeRunner 独立运行和结果收集
- 并发：前台任务 + 后台任务同时运行互不影响
- /bg 命令完整流程

## 风险

1. **并发 Claude 进程**：每个后台任务起独立 `claude -p`，消耗 API 配额和内存
2. **进程泄漏**：后台任务卡死不退出。建议设硬超时（如 60 分钟）
3. **结果推送失败**：Push API 调用失败。可存入离线队列重试
