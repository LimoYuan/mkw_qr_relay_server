#!/usr/bin/env bash
set -euo pipefail

APP_NAME="mkw-qr-relay"
INSTALL_DIR="/opt/${APP_NAME}"
CONFIG_DIR="/etc/${APP_NAME}"
DATA_DIR="/var/lib/${APP_NAME}"
SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"
DEFAULT_DOWNLOAD_BASE="https://github.com/LimoYuan/mkw_qr_relay_server/releases/download/v1/mkw-qr-relay-linux-amd64"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

red(){ echo -e "\033[31m$*\033[0m"; }
green(){ echo -e "\033[32m$*\033[0m"; }
yellow(){ echo -e "\033[33m$*\033[0m"; }

require_root(){
  if [[ "${EUID}" -ne 0 ]]; then
    red "请使用 root 执行，或使用：sudo bash install.sh"
    exit 1
  fi
}

ask(){
  local prompt="$1"
  local default="${2:-}"
  local value
  if [[ -n "$default" ]]; then
    read -r -p "${prompt} [${default}]: " value || true
    echo "${value:-$default}"
  else
    while true; do
      read -r -p "${prompt}: " value || true
      if [[ -n "$value" ]]; then echo "$value"; return; fi
      yellow "该项不能为空"
    done
  fi
}

normalize_site(){
  local v="$1"
  v="${v%/}"
  if [[ "$v" != http://* && "$v" != https://* ]]; then
    v="https://${v}"
  fi
  v="${v%/api/v4}"
  v="${v%/}"
  echo "$v"
}

host_from_url(){
  python3 - <<PY
from urllib.parse import urlparse
u=urlparse('$1')
print(u.hostname or '')
PY
}

rand_secret(){
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    tr -dc 'A-Za-z0-9' </dev/urandom | head -c 64
  fi
}

install_binary(){
  mkdir -p "$INSTALL_DIR"
  local local_bin="${SCRIPT_DIR}/mkw-qr-relay-linux-amd64"
  if [[ -f "$local_bin" ]]; then
    cp "$local_bin" "${INSTALL_DIR}/${APP_NAME}"
  else
    local base="${MKW_QR_RELAY_DOWNLOAD_BASE:-$DEFAULT_DOWNLOAD_BASE}"
    if [[ -z "$base" ]]; then
      echo
      yellow "未在当前目录找到 mkw-qr-relay-linux-amd64。"
      base="$(ask '请输入中转服务下载目录 URL，例如 https://download.example.com/mkw-qr-relay')"
    fi
    base="${base%/}"
    if command -v curl >/dev/null 2>&1; then
      curl -fL "${base}/mkw-qr-relay-linux-amd64" -o "${INSTALL_DIR}/${APP_NAME}"
    elif command -v wget >/dev/null 2>&1; then
      wget -O "${INSTALL_DIR}/${APP_NAME}" "${base}/mkw-qr-relay-linux-amd64"
    else
      red "缺少 curl/wget，无法下载程序"
      exit 1
    fi
  fi
  chmod +x "${INSTALL_DIR}/${APP_NAME}"
}

write_env(){
  mkdir -p "$CONFIG_DIR" "$DATA_DIR"
  cat > "${CONFIG_DIR}/relay.env" <<EOF
PUBLIC_URL=${PUBLIC_URL}
CLOUDREVE_URL=${CLOUDREVE_URL}
LISTEN_ADDR=${LISTEN_ADDR}:${LISTEN_PORT}
SESSION_EXPIRE_SECONDS=${SESSION_EXPIRE_SECONDS}
MAX_SESSIONS=${MAX_SESSIONS}
QR_SECRET=${QR_SECRET}
EOF
  chmod 600 "${CONFIG_DIR}/relay.env"
}

write_service(){
  cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=MKW QR Login Relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${CONFIG_DIR}/relay.env
ExecStart=${INSTALL_DIR}/${APP_NAME} -env ${CONFIG_DIR}/relay.env
Restart=always
RestartSec=3
WorkingDirectory=${DATA_DIR}
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$APP_NAME" >/dev/null
  systemctl restart "$APP_NAME"
}

nginx_location_block(){
  cat <<EOF

    # MKW 扫码登录中转服务
    location ${RELAY_PATH}/ {
        proxy_pass http://${LISTEN_ADDR}:${LISTEN_PORT}/;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
EOF
}

insert_nginx_location(){
  local conf="$1"
  if [[ ! -f "$conf" ]]; then
    red "Nginx 配置不存在：$conf"
    return 1
  fi
  if grep -q "location ${RELAY_PATH}/" "$conf"; then
    yellow "Nginx 已存在 ${RELAY_PATH}/ 配置，跳过写入"
    return 0
  fi
  cp "$conf" "${conf}.bak.$(date +%Y%m%d%H%M%S)"
  local block
  block="$(nginx_location_block)"
  python3 - "$conf" "$block" <<'PY'
import sys
path=sys.argv[1]
block=sys.argv[2]
text=open(path,encoding='utf-8').read()
idx=text.rfind('\n}')
if idx == -1:
    raise SystemExit('无法定位 server 结束括号')
text=text[:idx] + block + text[idx:]
open(path,'w',encoding='utf-8').write(text)
PY
  nginx -t
  systemctl reload nginx || service nginx reload
}

configure_nginx(){
  echo
  echo "是否自动写入 Nginx 反向代理路径？"
  echo "1) 自动检测宝塔/常见 Nginx 配置并写入"
  echo "2) 我手动输入 Nginx 站点配置文件路径"
  echo "3) 不配置 Nginx，只输出反代片段"
  local choice
  choice="$(ask '请选择' '1')"
  local domain
  domain="$(host_from_url "$CLOUDREVE_URL")"
  local candidates=(
    "/www/server/panel/vhost/nginx/${domain}.conf"
    "/etc/nginx/conf.d/${domain}.conf"
    "/etc/nginx/sites-available/${domain}"
  )
  case "$choice" in
    1)
      local found=""
      for c in "${candidates[@]}"; do
        if [[ -f "$c" ]]; then found="$c"; break; fi
      done
      if [[ -z "$found" ]]; then
        yellow "没有自动找到站点配置，将输出 Nginx 片段。"
        nginx_location_block
      else
        insert_nginx_location "$found"
      fi
      ;;
    2)
      local path
      path="$(ask '请输入 Nginx 站点配置文件完整路径')"
      insert_nginx_location "$path"
      ;;
    *)
      echo
      yellow "请把下面片段加入 Cloudreve 站点的 server {} 内："
      nginx_location_block
      ;;
  esac
}

require_root

echo "===================================================="
echo " MKW 扫码登录中转服务 - 交互式一键安装"
echo "===================================================="
echo

CLOUDREVE_INPUT="$(ask '请输入 Cloudreve 站点地址，例如 https://cloud.example.com')"
CLOUDREVE_URL="$(normalize_site "$CLOUDREVE_INPUT")"
RELAY_PATH="$(ask '请输入扫码中转路径' '/qr-login-relay')"
RELAY_PATH="/${RELAY_PATH#/}"
RELAY_PATH="${RELAY_PATH%/}"
PUBLIC_URL="${CLOUDREVE_URL}${RELAY_PATH}"
LISTEN_ADDR="$(ask '请输入本机监听地址' '127.0.0.1')"
LISTEN_PORT="$(ask '请输入监听端口' '8787')"
SESSION_EXPIRE_SECONDS="$(ask '二维码有效期秒数' '120')"
MAX_SESSIONS="$(ask '最大同时会话数' '5000')"
QR_SECRET="$(rand_secret)"

echo
echo "即将安装以下配置："
echo "Cloudreve 站点：${CLOUDREVE_URL}"
echo "扫码中转地址：${PUBLIC_URL}"
echo "监听地址：${LISTEN_ADDR}:${LISTEN_PORT}"
echo "二维码有效期：${SESSION_EXPIRE_SECONDS} 秒"
echo
read -r -p "确认开始安装？[Y/n]: " confirm || true
confirm="${confirm:-Y}"
if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
  yellow "已取消安装"
  exit 0
fi

install_binary
write_env
write_service
configure_nginx

echo
green "安装完成！"
echo "扫码登录中转服务地址：${PUBLIC_URL}"
echo "健康检查：${PUBLIC_URL}/api/health"
echo
echo "常用命令："
echo "systemctl status ${APP_NAME}"
echo "journalctl -u ${APP_NAME} -f"
echo "sudo bash uninstall.sh"
