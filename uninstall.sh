#!/usr/bin/env bash
set -euo pipefail
APP_NAME="mkw-qr-relay"
if [[ "${EUID}" -ne 0 ]]; then
  echo "请使用 root 执行，或使用：sudo bash uninstall.sh"
  exit 1
fi
systemctl stop "$APP_NAME" 2>/dev/null || true
systemctl disable "$APP_NAME" 2>/dev/null || true
rm -f "/etc/systemd/system/${APP_NAME}.service"
systemctl daemon-reload
rm -rf "/opt/${APP_NAME}"
read -r -p "是否删除配置和临时数据 /etc/${APP_NAME} /var/lib/${APP_NAME}？[y/N]: " ans || true
if [[ "${ans:-N}" =~ ^[Yy]$ ]]; then
  rm -rf "/etc/${APP_NAME}" "/var/lib/${APP_NAME}"
fi
echo "已卸载 ${APP_NAME}。如安装脚本写入过 Nginx location，请按备份文件手动恢复或删除相关片段。"
