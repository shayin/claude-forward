#!/bin/bash
# 部署脚本

set -e

INSTALL_DIR="/opt/claude-forward"
BIN_DIR="/usr/local/bin"

echo "=== Claude Forward 安装脚本 ==="

# 检查 root 权限
if [ "$EUID" -ne 0 ]; then
    echo "请使用 sudo 运行此脚本"
    exit 1
fi

# 创建目录
echo "创建目录..."
mkdir -p "$INSTALL_DIR"
mkdir -p /var/log/claude-forward

# 复制文件
echo "复制文件..."
cp bin/server "$INSTALL_DIR/"
cp bin/client "$INSTALL_DIR/"
cp configs/server.yaml "$INSTALL_DIR/"
cp -r web "$INSTALL_DIR/"

# 设置权限
chmod +x "$INSTALL_DIR/server"
chmod +x "$INSTALL_DIR/client"
chown -R nobody:nogroup "$INSTALL_DIR"
chown -R nobody:nogroup /var/log/claude-forward

# 安装 systemd 服务
echo "安装 systemd 服务..."
cp deploy/claude-forward-server.service /etc/systemd/system/
systemctl daemon-reload

echo ""
echo "=== 安装完成 ==="
echo ""
echo "下一步："
echo "1. 编辑配置文件: sudo vim $INSTALL_DIR/server.yaml"
echo "2. 启动服务: sudo systemctl start claude-forward-server"
echo "3. 设置开机启动: sudo systemctl enable claude-forward-server"
echo ""
