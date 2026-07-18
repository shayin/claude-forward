# Client 配置文件模板
# 复制为 client.yaml 后修改对应配置

# 服务器连接配置
server:
  url: "wss://your-server-ip:6022"   # 服务器 WebSocket 地址
  token: "your-secret-token-here"     # 认证 Token，需与 server.yaml 中的 tokens 一致
  reconnect_interval: 5               # 断线重连间隔（秒）
  encryption_key: ""                  # 应用层加密密钥（留空不加密，需与服务器一致，建议 32+ 字符随机字符串）

# 终端模式配置（用于 tmux 终端转发，聊天模式不需要）
tmux:
  session_name: ""                    # tmux session 名称，留空自动从项目目录推导
  auto_start: true                    # 是否自动创建 tmux session
  shell: ""                           # 默认 shell，留空使用系统默认

# Claude 聊天模式配置
claude:
  path: ""                            # claude 二进制路径，留空默认使用 PATH 中的 claude
  allowed_tools: "Bash,Read,Edit,Write,Glob,Grep"  # 允许 Claude 使用的工具
  max_turns: 50                       # 单次对话最大轮次
  env_file: ""                        # 可选，指向包含 export KEY=VALUE 的 shell 文件（用于 API 认证等）
                                      # 优先级高于 ~/.claude/settings.json 的 env 字段
  provider_dir: "~/.claude/providers" # 可选，providers 脚本目录，用于 /provider list 扫描和快速切换

# Codex CLI 引擎配置（微信通道通过 /engine codex 切换启用，Web UI 不使用 codex）
codex:
  path: "codex"                    # codex 二进制路径，留空默认使用 PATH 中的 codex
  sandbox: "workspace-write"       # sandbox 模式:
                                    #   workspace-write（默认，工作区可写但联网阻断）
                                    #   read-only（只读）
                                    #   dangerously-bypass-approvals-and-sandbox（无限制含联网，仅在自己机器上用）
  model: ""                        # 默认模型，留空用 codex 内置默认；微信可用 /model 运行时切换
  work_dir: ""                     # 可选工作目录，留空用当前目录（通常与 cc 一致）

# 本地 HTML 分享（默认关闭）。云端通过既有 WebSocket 长连接读取该目录中的静态资源。
# html_share:
#   root_dir: "~/claude-html-share" # 留空则关闭；仅允许此目录内的普通文件
#   public_base_url: ""             # 可选，如 https://example.com；留空由 server.url 推导
#   token_file: ""                  # 可选令牌保存位置；留空保存到项目 .claude-forward 目录
