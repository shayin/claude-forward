# Claude Forward 待办事项

## 1. 微信端工具调用过程回显

**问题**: 微信端执行 Claude 任务时，无法看到工具调用过程（如执行了什么命令、读取了什么文件），只有最终回复。

**原因**: 普通微信 iLink Bot API 不支持流式消息覆盖。企微有 `aibot_respond_msg` + `stream` 协议（同一 streamID 后发覆盖前发），但普通微信只有 `sendmessage`（每条独立，无法更新）。

**状态**: 等腾讯后续开放 iLink 流式消息能力。

**备选方案**:
- 方案 A: 多条消息展示过程（每次工具调用发一条中间消息，刷屏风险大）
- 方案 B: 最终回复中附带工具调用摘要（如 `[Read] main.go\n[Bash] go build\n\n回复内容...`）
- 方案 C: iLink 支持撤回后，用"撤回+重发"模拟流式效果

---

## 2. Web UI 权限审批功能

**问题**: 原设计通过 PreToolUse Hook + Web UI 弹窗实现工具调用审批，但实际上所有工具都被自动放行，审批形同虚设。

**原因**:
- 启动参数 `--dangerously-skip-permissions` 跳过了 Claude 内部权限
- `PermissionChecker.Check()` 对未匹配规则的工具默认返回 `ActionAllow`
- 用户的 `~/.claude/settings.json` 中通常没有 `ask` 规则

**建议方案**: 分级审批
- 安全操作（Read, Glob, Grep, WebSearch, WebFetch, LSP）→ 自动 Allow
- 危险操作（Bash, Edit, Write, NotebookEdit）→ Ask，发到 Web UI 审批
- 增加配置开关，支持 `all-allow`（当前行为）和 `ask-dangerous`（分级审批）

**涉及文件**:
- `internal/client/permissions.go` — `Check()` 默认行为改为按工具分级
- `internal/client/hook_server.go` — hooks-settings 中 permissions 规则调整
- Web UI — 添加权限审批弹窗组件

---

## 3. Web UI 暗黑模式

**问题**: Web UI 只有亮色主题，晚上使用刺眼。

**状态**: 待实现。
