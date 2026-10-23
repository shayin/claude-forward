#!/bin/bash
# 为当前 macOS 用户启用 cmux + CF Client 登录自启；不读取或复制服务端 Token。
set -euo pipefail
umask 077

if [[ $# -lt 1 || $# -gt 2 || "$1" != /* || ! -d "$1" ||
      ( $# -eq 2 && "$2" != "--no-load" ) ]]; then
  echo "用法: $0 /绝对项目目录 [--no-load]" >&2
  exit 2
fi

project="$1"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
client_dir="$HOME/.claude-forward"
launch_dir="$HOME/Library/LaunchAgents"
plist="$launch_dir/com.claude-forward.cmux-autostart.plist"
hook="$client_dir/cmux-client-autostart.zsh"
zshrc="$HOME/.zshrc"
marker="# >>> Claude Forward cmux auto-start >>>"

if [[ ! -x "$client_dir/client" || ! -f "$client_dir/client.yaml" ]]; then
  echo "请先安装 CF Client 并准备 ~/.claude-forward/client.yaml" >&2
  exit 1
fi
if [[ $# -eq 1 ]]; then
  [[ "$(uname -s)" == "Darwin" ]] || { echo "仅支持 macOS" >&2; exit 1; }
  /usr/bin/open -Ra cmux || { echo "请先安装 cmux" >&2; exit 1; }
fi

mkdir -p "$client_dir" "$launch_dir"
install -m 700 "$script_dir/cmux-client-autostart.zsh" "$hook"

if [[ ! -f "$zshrc" ]]; then
  touch "$zshrc"
fi
if ! grep -Fq "$marker" "$zshrc"; then
  cp -p "$zshrc" "$zshrc.cf-backup-$(date +%Y%m%d_%H%M%S)"
  printf -v quoted_project '%q' "$project"
  cat >>"$zshrc" <<EOF

$marker
if [[ -n \${CMUX_SOCKET_CAPABILITY:-} && -n \${CMUX_SOCKET_PATH:-} ]]; then
  nohup "\$HOME/.claude-forward/cmux-client-autostart.zsh" $quoted_project >/dev/null 2>&1 < /dev/null &!
fi
# <<< Claude Forward cmux auto-start <<<
EOF
fi

temp_plist="$(mktemp "$launch_dir/.cmux-autostart.XXXXXX")"
trap 'rm -f "$temp_plist"' EXIT
cat >"$temp_plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.claude-forward.cmux-autostart</string>
  <key>ProgramArguments</key>
  <array><string>/usr/bin/open</string><string>-a</string><string>cmux</string></array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
EOF
plutil -lint "$temp_plist" >/dev/null
mv "$temp_plist" "$plist"
chmod 600 "$plist"

if [[ $# -eq 1 ]]; then
  label="gui/$(id -u)/com.claude-forward.cmux-autostart"
  if ! launchctl print "$label" >/dev/null 2>&1; then
    launchctl bootstrap "gui/$(id -u)" "$plist"
  fi
fi
echo "已安装：登录后启动 cmux，cmux 终端将自动启动 CF Client（每次仅一份）。"
