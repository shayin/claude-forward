#!/bin/bash
# Claude Forward 微信桥接启动脚本
# 功能：编译 + 启动 wechat-bridge（前台运行，用于扫码登录）

set -e

# 设置 PATH
export PATH="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 获取脚本所在目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 配置
PID_FILE="/tmp/claude-forward-bridge.pid"
LOG_FILE="/tmp/claude-forward-bridge.log"
CONFIG_FILE="$SCRIPT_DIR/configs/wechat-bridge.yaml"
BINARY="$SCRIPT_DIR/bin/wechat-bridge"

print_info() { echo -e "${BLUE}[i]${NC} $1"; }
print_step() { echo -e "${GREEN}[✓]${NC} $1"; }
print_warn() { echo -e "${YELLOW}[!]${NC} $1"; }
print_error() { echo -e "${RED}[✗]${NC} $1"; }

# 检查进程是否运行
is_running() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if ps -p "$pid" > /dev/null 2>&1; then
            return 0
        fi
    fi
    return 1
}

# 停止进程
stop_process() {
    if is_running; then
        local pid=$(cat "$PID_FILE")
        print_info "正在停止 bridge (PID: $pid)..."
        kill "$pid" 2>/dev/null || true

        local count=0
        while ps -p "$pid" > /dev/null 2>&1 && [ $count -lt 10 ]; do
            sleep 0.5
            count=$((count + 1))
        done

        if ps -p "$pid" > /dev/null 2>&1; then
            kill -9 "$pid" 2>/dev/null || true
        fi

        rm -f "$PID_FILE"
        print_step "bridge 已停止"
    fi
}

# 拉取最新代码
pull() {
    print_info "正在拉取最新代码..."
    git pull origin main --no-rebase
    print_step "代码已更新"
}

# 编译
build() {
    print_info "正在编译 wechat-bridge..."
    make build-wechat-bridge
    print_step "编译完成"
}

# 启动（前台，用于扫码）
start() {
    if is_running; then
        print_warn "bridge 已在运行中 (PID: $(cat $PID_FILE))"
        exit 0
    fi

    # 检查配置文件
    if [ ! -f "$CONFIG_FILE" ]; then
        print_error "配置文件不存在: $CONFIG_FILE"
        print_info "请先创建配置文件，参考 configs/wechat-bridge.yaml"
        exit 1
    fi

    # 检查二进制文件
    if [ ! -f "$BINARY" ]; then
        print_warn "二进制文件不存在，正在编译..."
        build
    fi

    print_info "正在启动 wechat-bridge（前台模式，用于扫码登录）..."
    exec "$BINARY" "$CONFIG_FILE"
}

# 后台启动（用于 systemd/部署）
daemon() {
    if is_running; then
        print_warn "bridge 已在运行中 (PID: $(cat $PID_FILE))"
        exit 0
    fi

    if [ ! -f "$CONFIG_FILE" ]; then
        print_error "配置文件不存在: $CONFIG_FILE"
        exit 1
    fi

    if [ ! -f "$BINARY" ]; then
        build
    fi

    print_info "正在启动 wechat-bridge（后台模式）..."

    nohup "$BINARY" "$CONFIG_FILE" > "$LOG_FILE" 2>&1 &
    local pid=$!
    echo $pid > "$PID_FILE"

    sleep 1

    if ps -p "$pid" > /dev/null 2>&1; then
        print_step "bridge 已启动 (PID: $pid)"
        print_info "日志文件: $LOG_FILE"
        print_info "查看日志: tail -f $LOG_FILE"
    else
        print_error "bridge 启动失败，请检查日志: $LOG_FILE"
        rm -f "$PID_FILE"
        exit 1
    fi
}

# 拉取并重启
upgrade() {
    print_info "正在升级 bridge..."
    stop_process
    pull
    build
    daemon
}

# 重启
restart() {
    print_info "正在重启 bridge..."
    stop_process
    build
    daemon
}

# 状态
status() {
    if is_running; then
        local pid=$(cat "$PID_FILE")
        echo -e "${GREEN}●${NC} bridge 运行中 (PID: $pid)"
        echo "  日志: $LOG_FILE"
    else
        echo -e "${RED}○${NC} bridge 未运行"
    fi
}

# 日志
logs() {
    if [ -f "$LOG_FILE" ]; then
        tail -f "$LOG_FILE"
    else
        print_warn "日志文件不存在: $LOG_FILE"
    fi
}

# 帮助
help() {
    echo "用法: $0 <命令>"
    echo ""
    echo "命令:"
    echo "  start        编译并前台启动 bridge（用于扫码登录）"
    echo "  daemon       编译并后台启动 bridge"
    echo "  stop         停止 bridge"
    echo "  restart      重新编译并重启 bridge"
    echo "  upgrade      拉取最新代码并重启 bridge"
    echo "  status       查看 bridge 状态"
    echo "  logs         查看 bridge 日志"
    echo "  build        仅编译 bridge"
    echo "  pull         仅拉取最新代码"
    echo ""
}

# 主函数
case "${1:-}" in
    start)
        build
        start
        ;;
    daemon)
        build
        daemon
        ;;
    stop)
        stop_process
        ;;
    restart)
        restart
        ;;
    upgrade)
        upgrade
        ;;
    status)
        status
        ;;
    logs)
        logs
        ;;
    build)
        build
        ;;
    pull)
        pull
        ;;
    *)
        help
        exit 1
        ;;
esac
