# MKW QR Login Relay

这是 MKW 扫码登录中转服务。

## 安装

```bash
unzip -o mkw_qr_relay_server.zip -d mkw_qr_relay_server
cd mkw_qr_relay_server
sudo bash install.sh
```

脚本会询问 Cloudreve 站点地址、扫码中转路径、监听端口等配置。
默认同域名路径为：

```text
https://你的网盘域名/qr-login-relay
```



## 健康检查

```bash
curl -i http://127.0.0.1:8787/api/health
curl -i https://你的网盘域名/qr-login-relay/api/health
```
