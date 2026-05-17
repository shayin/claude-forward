# Server 配置文件模板
# 复制为 server.yaml 后修改对应配置

# 服务器监听配置
server:
  host: "0.0.0.0"                     # 监听地址
  port: 6022                          # 监听端口
  encryption_key: ""                  # 应用层加密密钥（留空不加密，需与客户端一致，建议 32+ 字符随机字符串）
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

# 微信集成（内置 iLink Bot 支持）
# 启用后 Server 直连 iLink API，无需单独运行 wechat-bridge
# 扫码通过 Web UI 或 API 完成
wechat:
  enabled: false                      # 是否启用微信集成
  data_dir: "wechat-data"             # 数据存储目录（session、bindings）
  push_secret: ""                     # Push API 密钥（留空不启用，填写后启用 POST /api/wechat/push）
  users:                              # 微信用户白名单路由（wechat_id → clawbot_id）
    # - wechat_id: "wxid_xxx@im.wechat"  # 微信用户 ID（首次从日志获取）
    #   clawbot_id: "my-macbook"          # 电脑级别 ID，对应 Client 的 clawbot_id
