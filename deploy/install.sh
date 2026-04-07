#!/bin/bash
# Claude Forward 一键部署脚本
# 适用于：Ubuntu/Debian/CentOS 等 Linux 系统

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 默认配置
INSTALL_DIR="/opt/claude-forward"
DEFAULT_PORT=6022
CONFIG_FILE=""

# 打印函数
print_header() {
    echo ""
    echo -e "${BLUE}╔══════════════════════════════════════════╗${NC}"
    echo -e "${BLUE}║       Claude Forward 部署脚本            ║${NC}"
    echo -e "${BLUE}║         自托管远程编程系统               ║${NC}"
    echo -e "${BLUE}╚══════════════════════════════════════════╝${NC}"
    echo ""
}

print_step() {
    echo -e "${GREEN}[✓]${NC} $1"
}

print_warn() {
    echo -e "${YELLOW}[!]${NC} $1"
}

print_error() {
    echo -e "${RED}[✗]${NC} $1"
}

print_info() {
    echo -e "${BLUE}[i]${NC} $1"
}

# 生成随机 Token
generate_token() {
    openssl rand -hex 32
}

# 检查 root 权限
check_root() {
    if [ "$EUID" -ne 0 ]; then
        print_error "请使用 sudo 运行此脚本"
        echo "  sudo $0"
        exit 1
    fi
}

# 检测系统类型
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS=$ID
    elif [ -f /etc/redhat-release ]; then
        OS="centos"
    else
        OS="unknown"
    fi
    print_info "检测到系统: $OS"
}

# 检查依赖
check_dependencies() {
    print_info "检查依赖..."

    local missing=()

    # 检查必要命令
    for cmd in openssl; do
        if ! command -v $cmd &> /dev/null; then
            missing+=($cmd)
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        print_warn "缺少依赖: ${missing[*]}"
        print_info "正在安装..."

        if [ "$OS" = "ubuntu" ] || [ "$OS" = "debian" ]; then
            apt-get update && apt-get install -y ${missing[*]}
        elif [ "$OS" = "centos" ] || [ "$OS" = "rhel" ]; then
            yum install -y ${missing[*]}
        else
            print_error "请手动安装依赖: ${missing[*]}"
            exit 1
        fi
    fi

    print_step "依赖检查完成"
}

# 查找编译好的二进制文件
find_binaries() {
    # 获取脚本所在目录
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

    if [ -f "$PROJECT_DIR/bin/server" ]; then
        BIN_DIR="$PROJECT_DIR/bin"
        WEB_DIR="$PROJECT_DIR/web"
        CONFIGS_DIR="$PROJECT_DIR/configs"
        print_info "找到项目目录: $PROJECT_DIR"
    elif [ -f "./bin/server" ]; then
        BIN_DIR="./bin"
        WEB_DIR="./web"
        CONFIGS_DIR="./configs"
        PROJECT_DIR="."
    else
        print_error "找不到编译好的二进制文件"
        print_info "请先运行: make build"
        exit 1
    fi
}

# 交互式配置
interactive_config() {
    echo ""
    echo -e "${YELLOW}>>> 配置向导${NC}"
    echo ""

    # 生成 Token
    local DEFAULT_TOKEN=$(generate_token)

    # Token 配置
    echo -e "${BLUE}Token 是访问系统的密钥，请妥善保管${NC}"
    read -p "请输入 Token (直接回车使用随机生成): " USER_TOKEN
    if [ -z "$USER_TOKEN" ]; then
        TOKEN="$DEFAULT_TOKEN"
        print_info "已生成随机 Token"
    else
        TOKEN="$USER_TOKEN"
    fi

    # 端口配置
    echo ""
    read -p "请输入监听端口 [默认 $DEFAULT_PORT]: " USER_PORT
    PORT=${USER_PORT:-$DEFAULT_PORT}

    # 服务器 IP
    echo ""
    echo -e "${BLUE}请输入服务器的公网 IP 地址${NC}"
    read -p "服务器 IP: " SERVER_IP

    echo ""
    echo -e "${YELLOW}配置摘要:${NC}"
    echo "  - 监听端口: $PORT"
    echo "  - Token: ${TOKEN:0:16}...${TOKEN: -8}"
    echo "  - 服务器 IP: $SERVER_IP"
    echo ""
    read -p "确认以上配置? [Y/n]: " CONFIRM
    if [ "$CONFIRM" = "n" ] || [ "$CONFIRM" = "N" ]; then
        print_error "已取消部署"
        exit 0
    fi
}

# 创建目录结构
create_directories() {
    print_info "创建目录结构..."

    mkdir -p "$INSTALL_DIR"
    mkdir -p /var/log/claude-forward

    print_step "目录创建完成"
}

# 安装文件
install_files() {
    print_info "安装文件..."

    # 复制二进制文件
    cp "$BIN_DIR/server" "$INSTALL_DIR/"
    cp "$BIN_DIR/client" "$INSTALL_DIR/"

    # 复制 Web UI
    cp -r "$WEB_DIR" "$INSTALL_DIR/"

    # 设置权限
    chmod +x "$INSTALL_DIR/server"
    chmod +x "$INSTALL_DIR/client"

    print_step "文件安装完成"
}

# 生成配置文件
generate_config() {
    print_info "生成配置文件..."

    # 从模板复制并替换变量
    if [ -f "$CONFIGS_DIR/server.yaml.tpl" ]; then
        sed -e "s/your-secret-token-here/$TOKEN/g" \
            -e "s/port: 6022/port: $PORT/g" \
            "$CONFIGS_DIR/server.yaml.tpl" > "$INSTALL_DIR/server.yaml"
    else
        # 如果模板不存在，生成默认配置
        cat > "$INSTALL_DIR/server.yaml" << EOF
# Claude Forward 服务器配置
# 生成时间: $(date)

server:
  host: "0.0.0.0"
  port: $PORT
  tls:
    enabled: true
    cert_file: ""
    key_file: ""
    domain: ""

auth:
  tokens:
    - "$TOKEN"

session:
  timeout: 300
  max_clients: 10

logging:
  enabled: true
  file: "/var/log/claude-forward/server.log"
  max_days: 7
  log_level: "info"
EOF
    fi

    # 设置权限
    chmod 600 "$INSTALL_DIR/server.yaml"

    print_step "配置文件生成完成"
}

# 生成客户端配置示例
generate_client_config() {
    sed -e "s|wss://your-server-ip:6022|wss://$SERVER_IP:$PORT|g" \
        -e "s/your-secret-token-here/$TOKEN/g" \
        "$CONFIGS_DIR/client.yaml.tpl" > "$INSTALL_DIR/client.yaml.example"

    print_step "客户端配置示例已生成"
}

# 设置文件所有者
set_permissions() {
    print_info "设置权限..."

    # 创建专用用户（如果不存在）
    if ! id -u claude-forward &>/dev/null; then
        useradd -r -s /bin/false claude-forward 2>/dev/null || true
    fi

    chown -R claude-forward:claude-forward "$INSTALL_DIR"
    chown -R claude-forward:claude-forward /var/log/claude-forward

    print_step "权限设置完成"
}

# 安装 systemd 服务
install_systemd_service() {
    print_info "安装 systemd 服务..."

    cat > /etc/systemd/system/claude-forward-server.service << EOF
[Unit]
Description=Claude Forward Server
After=network.target

[Service]
Type=simple
User=claude-forward
Group=claude-forward
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/server $INSTALL_DIR/server.yaml
Restart=always
RestartSec=5

# 安全设置
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/claude-forward

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload

    print_step "systemd 服务安装完成"
}

# 配置防火墙
configure_firewall() {
    echo ""
    print_info "配置防火墙..."

    if command -v ufw &> /dev/null; then
        # Ubuntu/Debian 使用 ufw
        if ufw status | grep -q "active"; then
            ufw allow $PORT/tcp comment 'Claude Forward'
            print_step "已添加 UFW 防火墙规则"
        else
            print_warn "UFW 未启用，请手动配置防火墙"
        fi
    elif command -v firewall-cmd &> /dev/null; then
        # CentOS/RHEL 使用 firewalld
        if systemctl is-active firewalld &>/dev/null; then
            firewall-cmd --permanent --add-port=$PORT/tcp
            firewall-cmd --reload
            print_step "已添加 firewalld 防火墙规则"
        else
            print_warn "firewalld 未启用，请手动配置防火墙"
        fi
    else
        print_warn "未检测到防火墙，请确保端口 $PORT 可访问"
    fi
}

# 打印部署完成信息
print_complete() {
    echo ""
    echo -e "${GREEN}╔══════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║           部署成功完成！                 ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${YELLOW}服务端操作:${NC}"
    echo "  启动服务:   sudo systemctl start claude-forward-server"
    echo "  停止服务:   sudo systemctl stop claude-forward-server"
    echo "  查看状态:   sudo systemctl status claude-forward-server"
    echo "  查看日志:   sudo journalctl -u claude-forward-server -f"
    echo "  设置开机启动: sudo systemctl enable claude-forward-server"
    echo ""
    echo -e "${YELLOW}客户端部署（在本地电脑执行）:${NC}"
    echo "  1. git clone https://github.com/shayin/claude-forward.git && cd claude-forward"
    echo "  2. make install-client"
    echo "  3. vim ~/.claude-forward/client.yaml  # 填写以下服务器信息"
    echo "     server.url: wss://$SERVER_IP:$PORT"
    echo "     server.token: $TOKEN"
    echo ""
    echo -e "${YELLOW}Web 访问:${NC}"
    echo "  https://$SERVER_IP:$PORT"
    echo ""
    echo -e "${YELLOW}重要信息:${NC}"
    echo "  Token: $TOKEN"
    echo ""
    echo -e "${YELLOW}注意事项:${NC}"
    echo "  1. 首次访问 Web 会提示证书不受信任，点击"高级" → "继续访问"即可"
    echo "  2. 请妥善保管 Token，这是访问系统的唯一凭证"
    echo "  3. 配置文件: $INSTALL_DIR/server.yaml"
    echo ""
}

# 主函数
main() {
    print_header

    # 检查权限
    check_root

    # 检测系统
    detect_os

    # 检查依赖
    check_dependencies

    # 查找二进制文件
    find_binaries

    # 交互式配置
    interactive_config

    # 执行安装
    create_directories
    install_files
    generate_config
    generate_client_config
    set_permissions
    install_systemd_service

    # 配置防火墙
    configure_firewall

    # 打印完成信息
    print_complete
}

# 运行主函数
main "$@"
