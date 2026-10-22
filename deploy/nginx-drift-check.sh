#!/bin/bash
# nebula.hoarfrost.cloud nginx 反代防漂移哨兵（由 NebulaFlow 运维安装）
# 每 5 分钟校验 /etc/nginx/conf.d/nebula.conf 是否仍含 /api 反代；
# 被其它部署流水线覆盖时自动恢复并 reload，记录到 /var/log/nebula-drift.log。
set -u
BASELINE=/opt/nebulaflow/nginx-drift-check.conf
TARGET=/etc/nginx/conf.d/nebula.conf
LOG=/var/log/nebula-drift.log

# web 根自适应：软链（/var/www/nebula/current）优先，其次平铺目录
webroot=/var/www/nebula/current
[ -e "$webroot" ] || webroot=/var/www/nebula

log() { echo "$(date '+%F %T') $*" >> "$LOG"; }

if grep -q 'location ^~ /api/' "$TARGET" 2>/dev/null; then
  exit 0   # 正常，无漂移
fi

log "ALERT: nebula.conf 丢失 /api 反代（可能被其它部署覆盖），开始恢复"
cp "$TARGET" "/tmp/nebula.conf.drift.$(date +%s)" 2>/dev/null   # 留档被覆盖的版本
sed "s|root /var/www/nebula/current;|root $webroot;|" "$BASELINE" > "$TARGET"
if nginx -t 2>>"$LOG"; then
  nginx -s reload && log "OK: 已恢复反代配置并 reload（webroot=$webroot）"
else
  log "ERROR: 恢复后的配置语法错误，请人工检查 $TARGET"
  exit 1
fi
