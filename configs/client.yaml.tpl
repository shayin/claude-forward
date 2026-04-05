# Client 配置文件模板
# 复制为 client.yaml 后修改对应配置

# 服务器连接配置
server:
  url: "wss://your-server-ip:6022"   # 服务器 WebSocket 地址
  token: "your-secret-token-here"     # 认证 Token，需与 server.yaml 中的 tokens 一致
  reconnect_interval: 5               # 断线重连间隔（秒）

# 终端模式配置（用于 tmux 终端转发，聊天模式不需要）
tmux:
  session_name: ""                    # tmux session 名称，留空自动生成
  auto_start: true                    # 是否自动创建 tmux session
  shell: ""                           # 默认 shell，留空使用系统默认

# Claude 聊天模式配置
claude:
  path: ""                            # claude 二进制路径，留空默认使用 PATH 中的 claude
  allowed_tools: "Bash,Read,Edit,Write,Glob,Grep"  # 允许 Claude 使用的工具
  max_turns: 50                       # 单次对话最大轮次
