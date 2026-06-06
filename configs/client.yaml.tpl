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
