#!/bin/bash
# Claude Forward 服务器启动脚本
# 功能：编译 + 启动服务器

set -e

# 设置 PATH（crontab 环境可能没有这些路径）
export PATH="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 配置
PID_FILE="/tmp/claude-forward-server.pid"
LOG_FILE="/tmp/claude-forward-server.log"
CONFIG_TPL="configs/server.yaml.tpl"
CONFIG_FILE="configs/server.yaml"
BINARY="bin/server"

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
        print_info "正在停止服务器 (PID: $pid)..."
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
        print_step "服务器已停止"
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
    print_info "正在编译服务器..."
    make build-server
    print_step "编译完成"
}

# 启动
start() {
    if is_running; then
        print_warn "服务器已在运行中 (PID: $(cat $PID_FILE))"
        exit 0
    fi

    # 检查配置文件
    if [ ! -f "$CONFIG_FILE" ]; then
        if [ -f "$CONFIG_TPL" ]; then
            print_info "从模板创建配置文件..."
            cp "$CONFIG_TPL" "$CONFIG_FILE"
            print_step "配置文件已创建: $CONFIG_FILE"
            print_warn "请编辑配置文件填写你的配置"
        else
            print_error "配置模板不存在: $CONFIG_TPL"
            exit 1
        fi
    fi

    # 检查二进制文件
    if [ ! -f "$BINARY" ]; then
        print_warn "二进制文件不存在，正在编译..."
        build
    fi

    print_info "正在启动服务器..."

    # 后台启动
    nohup "$BINARY" "$CONFIG_FILE" > "$LOG_FILE" 2>&1 &
    local pid=$!
    echo $pid > "$PID_FILE"

    sleep 1

    if ps -p "$pid" > /dev/null 2>&1; then
        print_step "服务器已启动 (PID: $pid)"
        print_info "日志文件: $LOG_FILE"
        print_info "查看日志: tail -f $LOG_FILE"
    else
        print_error "服务器启动失败，请检查日志: $LOG_FILE"
        rm -f "$PID_FILE"
        exit 1
    fi
}

# 拉取并重启
upgrade() {
    print_info "正在升级服务器..."
    stop_process
    pull
    build
    start
}

# 自动升级（用于定时任务，只有发现更新才执行）
auto_upgrade() {
    # 静默模式，不输出颜色
    print_info "检查更新..."

    # 存活检测：server 未运行则自动拉起（用现有 bin，不 build）
    # 解决 server 崩溃/被误杀时 auto-upgrade 只看 git 更新、不拉起的问题
    if ! is_running; then
        print_warn "server 未运行，自动拉起..."
        start
    fi

    # 获取当前 commit
    local current_commit=$(git rev-parse HEAD 2>/dev/null)
    if [ -z "$current_commit" ]; then
        print_error "无法获取当前版本"
        exit 1
    fi

    # 拉取远程信息（不合并）
    git fetch origin main --quiet 2>/dev/null

    # 获取远程最新 commit
    local remote_commit=$(git rev-parse origin/main 2>/dev/null)
    if [ -z "$remote_commit" ]; then
        print_error "无法获取远程版本"
        exit 1
    fi

    # 比较版本
    if [ "$current_commit" = "$remote_commit" ]; then
        print_info "已是最新版本，无需更新"
        exit 0
    fi

    # 有更新，执行升级
    print_info "发现新版本，正在升级..."
    echo "  本地: ${current_commit:0:7}"
    echo "  远程: ${remote_commit:0:7}"

    stop_process
    git pull origin main --no-rebase --quiet
    build
    start

    print_step "升级完成"
}

# 重启
restart() {
    print_info "正在重启服务器..."
    stop_process
    build
    start
}

# 状态
status() {
    if is_running; then
        local pid=$(cat "$PID_FILE")
        echo -e "${GREEN}●${NC} 服务器运行中 (PID: $pid)"
        echo "  日志: $LOG_FILE"
    else
        echo -e "${RED}○${NC} 服务器未运行"
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
    echo "  start        编译并启动服务器"
    echo "  stop         停止服务器"
    echo "  restart      重新编译并重启服务器"
    echo "  upgrade      拉取最新代码并重启服务器"
    echo "  auto-upgrade 检查更新，有新版本才升级（用于定时任务）"
    echo "  status       查看服务器状态"
    echo "  logs         查看服务器日志"
    echo "  build        仅编译服务器"
    echo "  pull         仅拉取最新代码"
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
    upgrade)
        upgrade
        ;;
    auto-upgrade)
        auto_upgrade
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
