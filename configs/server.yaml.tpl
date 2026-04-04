# Server 配置文件模板
# 复制为 server.yaml 后修改对应配置

# 服务器监听配置
server:
  host: "0.0.0.0"                     # 监听地址
  port: 6022                          # 监听端口
  tls:
    enabled: false                    # 是否启用 TLS（生产环境建议开启）
    cert_file: ""                     # TLS 证书文件路径
    key_file: ""                      # TLS 私钥文件路径
    domain: ""                        # 域名，填写后自动使用 Let's Encrypt 签发证书

# 认证配置
auth:
  tokens:                             # 允许连接的 Token 列表，客户端需携带其中一个
    - "your-secret-token-here"

# 会话配置
session:
  timeout: 300                        # 会话超时（秒），无活动自动断开
  max_clients: 10                     # 最大允许连接的客户端数

# 日志配置
logging:
  enabled: true                       # 是否启用日志
  file: "/var/log/claude-forward/server.log"  # 日志文件路径
  max_days: 7                         # 日志保留天数
  log_level: "info"                   # 日志级别: debug, info, warn, error
