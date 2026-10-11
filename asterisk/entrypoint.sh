#!/bin/sh
# 環境変数から設定を作って Asterisk を起動する。
set -eu

: "${ARI_PASSWORD:?ARI_PASSWORD を設定してください}"
export ARI_USER="${ARI_USER:-myvoip}"
export ARI_BIND_ADDRESS="${ARI_BIND_ADDRESS:-127.0.0.1}"
export RELAY_SIP_PORT="${RELAY_SIP_PORT:-5060}"
export RELAY_RTP_START="${RELAY_RTP_START:-10000}"
export RELAY_RTP_END="${RELAY_RTP_END:-10999}"

for template in /etc/myvoip-asterisk/templates/*.conf; do
  envsubst '${ARI_USER} ${ARI_PASSWORD} ${ARI_BIND_ADDRESS} ${RELAY_SIP_PORT} ${RELAY_RTP_START} ${RELAY_RTP_END}' \
    < "$template" > "/etc/asterisk/$(basename "$template")"
done
chmod 640 /etc/asterisk/ari.conf
chown root:asterisk /etc/asterisk/ari.conf

# myvoip-server（uid 65532）が内線の設定を書き出す共有ディレクトリ
mkdir -p /etc/asterisk/myvoip
chown 65532:65532 /etc/asterisk/myvoip
chmod 755 /etc/asterisk/myvoip

# テスト着信の録音の保存先
mkdir -p /var/spool/asterisk/recording
chown -R asterisk:asterisk /var/spool/asterisk /var/lib/asterisk /var/log/asterisk /var/run/asterisk 2>/dev/null || true

exec /usr/sbin/asterisk -f -U asterisk -G asterisk -vvv
