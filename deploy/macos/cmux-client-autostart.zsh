#!/bin/zsh
# 只能由 cmux 中的 shell 启动：保留 cmux 赋予子进程的 socket 访问权限。
set -eu
umask 077

if [[ -z "${CMUX_SOCKET_CAPABILITY:-}" || -z "${CMUX_SOCKET_PATH:-}" ||
      ! -S "$CMUX_SOCKET_PATH" ]]; then
  exit 0
fi

managed=false
if [[ "${1:-}" == "--managed" ]]; then
  managed=true
  shift
fi
if [[ $# -lt 1 || $# -gt 2 || ( $managed == true && $# -ne 2 ) || "$1" != /* ||
      ( $# -eq 2 && "$2" != <-> ) ]]; then
  print -u2 "需要一个绝对项目目录"
  exit 2
fi

client="$HOME/.claude-forward/client"
config="$HOME/.claude-forward/client.yaml"
project="$1"
state="$HOME/.claude-forward"

if [[ ! -x "$client" || ! -f "$config" || ! -d "$project" ]]; then
  print -u2 "CF Client、配置文件或项目目录不存在"
  exit 1
fi

mkdir -p "$state"
if $managed; then
  parent_pid="$2"
  # shell 退出后 cmux 会撤销该终端的 socket 权限；及时结束旧 Client，
  # 防止它继续以“在线但终端不可用”的状态占住单实例锁。
  "$client" -config "$config" -dir "$project" &
  client_pid=$!
  (
    while kill -0 "$parent_pid" 2>/dev/null && kill -0 "$client_pid" 2>/dev/null; do
      sleep 2
    done
    if ! kill -0 "$parent_pid" 2>/dev/null; then
      kill -TERM "$client_pid" 2>/dev/null || true
    fi
  ) &
  watcher_pid=$!
  trap 'kill "$watcher_pid" 2>/dev/null || true; kill -TERM "$client_pid" 2>/dev/null || true' EXIT
  exit_code=0
  wait "$client_pid" || exit_code=$?
  exit "$exit_code"
fi

parent_pid="${2:-$PPID}"
# lockf 锁随 Client 进程退出自动释放；每个 cmux 终端都可触发，但不会启动第二份。
exec /usr/bin/lockf -kst 0 "$state/cmux-client.lock" \
  "$0" --managed "$project" "$parent_pid" >>"$state/cmux-client.log" 2>&1
