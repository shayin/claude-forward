#!/bin/bash
# Claude Forward 客户端启动脚本
# 功能：编译 + 启动客户端

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 配置
PID_FILE="/tmp/claude-forward-client.pid"
LOG_FILE="/tmp/claude-forward-client.log"
CONFIG_FILE="configs/client.yaml"
BINARY="bin/client"

# 获取脚本所在目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

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
        print_info "正在停止客户端 (PID: $pid)..."
        kill "$pid" 2>/dev/null || true

        # 等待进程结束
        local count=0
        while ps -p "$pid" > /dev/null 2>&1 && [ $count -lt 10 ]; do
            sleep 0.5
            count=$((count + 1))
        done

        # 强制杀死
        if ps -p "$pid" > /dev/null 2>&1; then
            kill -9 "$pid" 2>/dev/null || true
        fi

        rm -f "$PID_FILE"
        print_step "客户端已停止"
    fi
}

# 编译
build() {
    print_info "正在编译客户端..."
    make build-client
    print_step "编译完成"
}

# 启动
start() {
    if is_running; then
        print_warn "客户端已在运行中 (PID: $(cat $PID_FILE))"
        exit 0
    fi

    # 检查配置文件
    if [ ! -f "$CONFIG_FILE" ]; then
        print_error "配置文件不存在: $CONFIG_FILE"
        print_info "请先创建配置文件: cp configs/client.yaml.example $CONFIG_FILE"
        exit 1
    fi

    # 检查二进制文件
    if [ ! -f "$BINARY" ]; then
        print_warn "二进制文件不存在，正在编译..."
        build
    fi

    print_info "正在启动客户端..."

    # 后台启动
    nohup "$BINARY" -config "$CONFIG_FILE" > "$LOG_FILE" 2>&1 &
    local pid=$!
    echo $pid > "$PID_FILE"

    sleep 1

    if ps -p "$pid" > /dev/null 2>&1; then
        print_step "客户端已启动 (PID: $pid)"
        print_info "日志文件: $LOG_FILE"
        print_info "查看日志: tail -f $LOG_FILE"
    else
        print_error "客户端启动失败，请检查日志: $LOG_FILE"
        rm -f "$PID_FILE"
        exit 1
    fi
}

# 重启
restart() {
    print_info "正在重启客户端..."
    stop_process
    build
    start
}

# 状态
status() {
    if is_running; then
        local pid=$(cat "$PID_FILE")
        echo -e "${GREEN}●${NC} 客户端运行中 (PID: $pid)"
        echo "  日志: $LOG_FILE"
    else
        echo -e "${RED}○${NC} 客户端未运行"
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
    echo "  start     编译并启动客户端"
    echo "  stop      停止客户端"
    echo "  restart   重新编译并重启客户端"
    echo "  status    查看客户端状态"
    echo "  logs      查看客户端日志"
    echo "  build     仅编译客户端"
    echo ""
}

# 主函数
case "${1:-}" in
    start)
        build
        start
        ;;
    stop)
        stop_process
        ;;
    restart)
        restart
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
    *)
        help
        exit 1
        ;;
esac
