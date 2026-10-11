# 中継用 Asterisk（中継モード）
# MikoPBX に内線として常時登録し、iPhone の登録先になる。呼制御は myvoip-server が ARI で行う。
FROM ubuntu:24.04

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      asterisk asterisk-core-sounds-en-gsm asterisk-moh-opsound-gsm gettext-base ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY templates/ /etc/myvoip-asterisk/templates/
COPY entrypoint.sh /usr/local/bin/myvoip-asterisk-entrypoint
RUN chmod 755 /usr/local/bin/myvoip-asterisk-entrypoint

HEALTHCHECK --interval=15s --timeout=5s --retries=3 CMD asterisk -rx "core show uptime" >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/myvoip-asterisk-entrypoint"]
