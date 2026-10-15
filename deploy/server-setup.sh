#!/usr/bin/env bash
#
# NebulaFlow 服务器端安装/升级脚本（幂等）
#
# 安装位置：/opt/nebulaflow/server-setup.sh（由 CI scp 上去后以 root 执行）
#
# 它按**固定顺序**做五件事，顺序本身就是这个脚本的主要价值：
#   1. 基础设施：确保 docker compose 可用 → 确保镜像存在 → up -d → 等到 healthy
#   2. 二进制：把 staging 里的 linux/amd64 二进制装到 /opt/nebulaflow/bin（原子替换）
#   3. 配置：/opt/nebulaflow/.env 不存在则生成（含随机密钥），存在则**只补缺失的键**
#   4. 服务：装 systemd unit → daemon-reload → enable → restart → 等 /healthz 200
#   5. 演示数据：**每次部署**都跑一遍 nebulaflow-seed（幂等），失败只告警不阻断；
#      .seeded 只是"最近一次 seed 成功时间"的标记，不再决定是否执行
#
# 顺序的理由：数据库必须先于进程就绪（否则进程连不上库直接退出），
# 进程必须先于 nginx reload（否则 nginx 会把 /api 反代到一个连接被拒的端口），
# 而这三者又都必须先于前端切换 —— 否则会出现"前端要真实 API、后端还没起"的空窗。
# CI 那边还会在这一步成功之后再 reload nginx、再切前端。
#
# 幂等性要求：重复执行不改变已有状态、不丢数据、不重置密钥。
#
# 环境变量（全部可选）：
#   NEBULA_OPT_DIR        默认 /opt/nebulaflow
#   NEBULA_STAGING_DIR    默认 /tmp/nebula-deploy（CI scp 上来的文件）
#   NEBULA_API_PORT       默认 18080（8080 被服务器上 nginx 的 blog vhost 占用了，见下）
#   NEBULA_PG_PORT        默认 15432
#   NEBULA_REDIS_PORT     默认 16379
#   NEBULA_IMAGE_MIRRORS  默认 "docker.1panel.live docker.m.daocloud.io dockerproxy.net"
#   NEBULA_HEALTH_TIMEOUT 默认 180（秒）

set -euo pipefail

OPT_DIR="${NEBULA_OPT_DIR:-/opt/nebulaflow}"
STAGING_DIR="${NEBULA_STAGING_DIR:-/tmp/nebula-deploy}"
API_PORT="${NEBULA_API_PORT:-18080}"
PG_PORT="${NEBULA_PG_PORT:-15432}"
REDIS_PORT="${NEBULA_REDIS_PORT:-16379}"
HEALTH_TIMEOUT="${NEBULA_HEALTH_TIMEOUT:-180}"

BIN_DIR="$OPT_DIR/bin"
ENV_FILE="$OPT_DIR/.env"
COMPOSE_FILE="$OPT_DIR/docker-compose.server.yml"
UNIT_FILE="/etc/systemd/system/nebulaflow.service"
SEED_MARKER="$OPT_DIR/.seeded"

log()  { printf '[server-setup] %s\n' "$*"; }
warn() { printf '[server-setup][WARN] %s\n' "$*" >&2; }
die()  { printf '[server-setup][ERROR] %s\n' "$*" >&2; exit 1; }

# ---------- 0. 权限：CI 的 DEPLOY_USER 大概率是 root，但不假设 ----------
# 不是 root 且 sudo -n 不可用就直接失败：这个脚本要写 /etc/systemd/system、
# 要装 apt 包、要 chmod 600 一个含密钥的文件，没有半途降级的版本。
if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
  log "running as root (uid=0)"
else
  if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    SUDO="sudo -n"
    log "running as $(id -un) (uid=$(id -u)); using 'sudo -n' for privileged steps"
  else
    die "需要 root 权限，但当前用户 $(id -un) 既不是 root，也无法免密 sudo。
    请把 CI 的 DEPLOY_USER 设为 root，或给该用户配置 NOPASSWD sudo。"
  fi
fi

# $SUDO 有意不加引号：为空时应当整个消失（word splitting 正是这里想要的效果）
# shellcheck disable=SC2086
run_root() { $SUDO "$@"; }
# docker 也走同一层：非 root 用户通常也不在 docker 组里。
# 注意不要把它包成 "dk() {  $SUDO docker "$@"; }" 然后交给 timeout ——
# timeout 只能执行外部命令，不能执行 shell 函数。
# shellcheck disable=SC2086
compose() { $SUDO $COMPOSE -f "$COMPOSE_FILE" --project-directory "$OPT_DIR" "$@"; }

# 读 /opt/nebulaflow/.env：非 root 时文件是 0600 root:root，必须借 sudo 读
env_read() {
  if [ -r "$ENV_FILE" ]; then cat "$ENV_FILE"; else run_root cat "$ENV_FILE"; fi
}
env_append() {
  printf '%s\n' "$1" | run_root tee -a "$ENV_FILE" >/dev/null
}

[ -d "$STAGING_DIR" ] || die "staging 目录不存在: $STAGING_DIR（CI 的 scp 步骤没跑或路径不对）"

# ---------- 1a. docker compose ----------
# 服务器上的 docker 29.1.3 只带了 docker-trust 插件，没有 compose。
# 没有它就退化成手写 docker run（卷/端口/健康检查全要自己维护），
# 所以这里用 apt 装官方插件 docker-compose-v2 —— 注意是从 apt（阿里云镜像）装，
# 不是从 Docker Hub 拉镜像（那台机器上 Docker Hub 是连不通的）。
COMPOSE=""
if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
  log "compose available: $(docker compose version)"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
  log "docker compose 插件不可用，退回旧版 docker-compose（$(docker-compose version --short 2>/dev/null || echo unknown)）"
else
  log "docker compose 插件缺失，从 apt 安装 docker-compose-v2 ..."
  run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-compose-v2 \
    || die "安装 docker-compose-v2 失败（apt 源不可达？）"
  docker compose version >/dev/null 2>&1 || die "docker-compose-v2 安装后仍不可用"
  COMPOSE="docker compose"
  log "installed: $(docker compose version)"
fi

# ---------- 1b. 镜像 ----------
# 这台服务器连不上 Docker Hub（实测 registry-1.docker.io 连接被拒），
# 所以"拉不到就走镜像代理"必须是脚本的一部分，而不是让运维现场想办法。
# 已经在本地的镜像直接跳过 —— 既省时间，也保证重复部署不会因为远端 tag 漂移
# 而换掉正在跑的基础设施版本。
ensure_image() {
  local canonical="$1" repo tag mirror
  if run_root docker image inspect "$canonical" >/dev/null 2>&1; then
    log "image present: $canonical"
    return 0
  fi
  log "image missing, pulling $canonical ..."
  if run_root timeout 300 docker pull "$canonical" >/dev/null 2>&1; then
    log "pulled $canonical"
    return 0
  fi
  warn "直接拉取 $canonical 失败（Docker Hub 不可达？），改走镜像代理"
  repo="${canonical%%:*}"
  tag="${canonical##*:}"
  case "$repo" in
    */*) ;;                      # 官方库需要补 library/ 前缀，例如 redis -> library/redis
    *)   repo="library/$repo" ;;
  esac
  for mirror in ${NEBULA_IMAGE_MIRRORS:-docker.1panel.live docker.m.daocloud.io dockerproxy.net}; do
    log "  trying mirror $mirror"
    if run_root timeout 900 docker pull "$mirror/$repo:$tag"; then
      run_root docker tag "$mirror/$repo:$tag" "$canonical"
      log "  ok: $mirror/$repo:$tag -> $canonical"
      return 0
    fi
    warn "  mirror $mirror failed"
  done
  die "无法获取镜像 $canonical（直连与所有镜像代理都失败）"
}

ensure_image "pgvector/pgvector:pg16"
ensure_image "redis:7-alpine"

# ---------- 1c. 目录 + compose 文件 + .env（.env 必须先于 up -d 存在） ----------
run_root install -d -m 0755 "$OPT_DIR"
run_root install -d -m 0755 "$BIN_DIR"
run_root install -m 0644 "$STAGING_DIR/docker-compose.server.yml" "$COMPOSE_FILE"

gen_secret() { openssl rand -hex "$1"; }

if [ ! -f "$ENV_FILE" ]; then
  log "生成新的 $ENV_FILE（首次部署）：JWT_SECRET / ENCRYPTION_KEY / POSTGRES_PASSWORD 本次随机生成"
  PG_PASS="$(gen_secret 16)"
  JWT_SECRET="$(gen_secret 32)"        # 64 个 hex 字符 = 64 字节 > minSecretLen(32)
  ENCRYPTION_KEY="$(gen_secret 16)"    # 32 个字符 = 正好 32 字节，AES-256 要求
  TMP_ENV="$(mktemp)"
  umask 077
  cat > "$TMP_ENV" <<EOF
# NebulaFlow 服务器环境变量 —— 由 deploy/server-setup.sh 生成，权限 0600 root:root。
# ⚠ 这个文件里的 JWT_SECRET / ENCRYPTION_KEY / POSTGRES_PASSWORD **只在首次生成**：
#   覆盖 JWT_SECRET      = 所有已签发 token 立刻失效（用户被登出）
#   覆盖 ENCRYPTION_KEY  = 库里已加密的 provider api key 全部解不开，
#                          服务启动时会 fail-closed 直接拒绝启动（见 cmd/server）
#   覆盖 POSTGRES_PASSWORD = 与已有 PG 数据卷里的密码不一致，连不上库
# 要轮换密钥请按"先解密→换钥→重新加密"的流程单独做，不要直接改这里。

# --- 基础 ---
APP_ENV=prod
# 18080 而不是 8080：服务器上 nginx 的 blog vhost 已经 listen 0.0.0.0:8080，
# 后端绑不上（实测 127.0.0.1:8080 也 EADDRINUSE，因为 0.0.0.0 覆盖了回环地址）。
PORT=$API_PORT

# --- 数据库（只在本机回环上，公网不可达） ---
DATABASE_URL=postgres://nebula:${PG_PASS}@127.0.0.1:${PG_PORT}/nebulaflow?sslmode=disable
POSTGRES_PASSWORD=${PG_PASS}
REDIS_ADDR=127.0.0.1:${REDIS_PORT}
REDIS_PASSWORD=

# --- 认证 / 加密（随机生成，仅此一份） ---
JWT_SECRET=${JWT_SECRET}
JWT_EXPIRE_HOURS=72
ENCRYPTION_KEY=${ENCRYPTION_KEY}

# --- Worker / 队列 ---
# 1.6G 内存的小机器：worker 并发与连接池都往下压。
# 默认 WORKER_COUNT=20 + DB_MAX_CONNS=25 在这个规格上只会互相排队。
WORKER_COUNT=8
CONSUMER_COUNT=0
QUEUE_NAME=workflow_tasks
QUEUE_MAXLEN=10000
QUEUE_DLQ_MAXLEN=1000
QUEUE_ADMIT_RATIO=0.8
USE_REDIS_QUEUE=true
WORKFLOW_LOCK_SCOPE=task

# --- 连接池（必须 <= PG 的 max_connections=60，且 >= WORKER_COUNT） ---
DB_MAX_CONNS=10
DB_MIN_CONNS=2
DB_MAX_CONN_LIFETIME=1h
DB_MAX_CONN_IDLE_TIME=30m
DB_CONNECT_TIMEOUT=5s

# --- 限流 ---
RATE_LIMIT_PER_MIN=120
TASK_RATE_LIMIT_PER_MIN=60
AUTH_RATE_LIMIT_PER_MIN=10

# --- 任务 ---
TASK_TIMEOUT_SEC=300
DEFAULT_MAX_RETRY=2

# --- 向量 ---
EMBED_DIM=384
OLLAMA_URL=http://127.0.0.1:11434
OLLAMA_EMBED_MODEL=
INDEX_CONCURRENCY=1

# --- 存储 / 缓存 / 迁移 ---
STORAGE_MODE=postgres
WORKFLOW_CACHE_TTL=30s
MIGRATE_ON_STARTUP=true

# --- LLM ---
LLM_MOCK_TOOL_CALL=true

# --- CORS：同源部署（nginx 把 /api 反代到本进程），收紧到站点域名 ---
CORS_ORIGINS=https://nebula.hoarfrost.cloud

# --- Trace ---
OTEL_ENABLED=false
OTEL_SAMPLE_RATIO=1
OTEL_EXPORTER_OTLP_ENDPOINT=
OTEL_MAX_TRACES=256
OTEL_MAX_SPANS_PER_TRACE=200
EOF
  umask 022
  run_root install -m 0600 -o root -g root "$TMP_ENV" "$ENV_FILE"
  rm -f "$TMP_ENV"
  log "wrote $ENV_FILE (mode 600, root:root)"
else
  log "$ENV_FILE 已存在，保留现有内容（不覆盖任何已有键，尤其是三处密钥）"
  if ! env_read | grep -q '^POSTGRES_PASSWORD=..*'; then
    die "$ENV_FILE 存在但缺少 POSTGRES_PASSWORD：PG 数据卷里已有旧密码，这里不能凭空生成一个。
    请手动补一行 POSTGRES_PASSWORD=<现有密码> 后重跑。"
  fi
  run_root chmod 600 "$ENV_FILE"
fi

ENV_PG_PASS="$(env_read | grep -m1 '^POSTGRES_PASSWORD=' | cut -d= -f2-)"
[ -n "$ENV_PG_PASS" ] || die "$ENV_FILE 中 POSTGRES_PASSWORD 为空"

# 幂等补齐：只在键**完全不存在**时追加，绝不修改已有行。
ensure_env_key() {
  local key="$1" value="$2"
  if env_read | grep -q "^${key}="; then
    return 0
  fi
  log "  .env 缺少 $key，补上默认值"
  env_append "${key}=${value}"
}
ensure_env_key APP_ENV prod
ensure_env_key PORT "$API_PORT"
ensure_env_key DATABASE_URL "postgres://nebula:${ENV_PG_PASS}@127.0.0.1:${PG_PORT}/nebulaflow?sslmode=disable"
ensure_env_key REDIS_ADDR "127.0.0.1:${REDIS_PORT}"
ensure_env_key REDIS_PASSWORD ""
ensure_env_key STORAGE_MODE postgres
ensure_env_key MIGRATE_ON_STARTUP true
ensure_env_key CORS_ORIGINS "https://nebula.hoarfrost.cloud"
ensure_env_key EMBED_DIM 384
ensure_env_key WORKFLOW_LOCK_SCOPE task
ensure_env_key QUEUE_ADMIT_RATIO 0.8
# JWT_SECRET / ENCRYPTION_KEY 刻意**不**在这里补：缺失时应该由服务自己拒绝启动
# （config.Validate 在 APP_ENV != dev 时把空 JWT_SECRET/ENCRYPTION_KEY 判为致命），
# 而不是被脚本悄悄塞一个随机值 —— 那会让"密钥丢了"看起来像"一切正常"。

# ---------- 1d. 起基础设施并等 healthy ----------
log "docker compose up -d ..."
compose up -d --remove-orphans

wait_healthy() {
  local svc="$1" waited=0 cid state
  while [ "$waited" -lt "$HEALTH_TIMEOUT" ]; do
    cid="$(compose ps -q "$svc" 2>/dev/null || true)"
    if [ -n "$cid" ]; then
      state="$(run_root docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || echo unknown)"
      case "$state" in
        healthy)   log "$svc: healthy (after ${waited}s)"; return 0 ;;
        unhealthy) compose logs --tail 40 "$svc" >&2 || true; die "$svc 进入 unhealthy 状态" ;;
      esac
    fi
    sleep 3
    waited=$((waited + 3))
  done
  compose logs --tail 40 "$svc" >&2 || true
  die "$svc 在 ${HEALTH_TIMEOUT}s 内没有变成 healthy"
}
wait_healthy postgres
wait_healthy redis

# ---------- 2. 安装二进制（原子替换） ----------
# 必须"先写临时文件再 mv"：直接 install 覆盖正在执行的二进制会 ETXTBSY；
# rename 是换 inode，正在跑的进程继续用旧 inode，systemd restart 之后才切到新的。
for b in nebulaflow-server nebulaflow-seed; do
  [ -f "$STAGING_DIR/bin/$b" ] || die "staging 缺少二进制: $STAGING_DIR/bin/$b"
  run_root install -m 0755 "$STAGING_DIR/bin/$b" "$BIN_DIR/.$b.new"
  run_root mv -f "$BIN_DIR/.$b.new" "$BIN_DIR/$b"
  log "installed $BIN_DIR/$b ($(run_root stat -c '%s bytes' "$BIN_DIR/$b"))"
done

# ---------- 3. systemd ----------
[ -f "$STAGING_DIR/nebulaflow.service" ] || die "staging 缺少 nebulaflow.service"
run_root install -m 0644 "$STAGING_DIR/nebulaflow.service" "$UNIT_FILE"
run_root systemctl daemon-reload
run_root systemctl enable nebulaflow >/dev/null
log "restarting nebulaflow ..."
run_root systemctl restart nebulaflow

# ---------- 4. 等 /healthz（失败必须阻断，绝不能带着还没起的后端去切前端） ----------
wait_healthz() {
  local url="http://127.0.0.1:${API_PORT}/healthz" waited=0
  while [ "$waited" -lt "$HEALTH_TIMEOUT" ]; do
    if curl -fsS -m 3 -o /dev/null "$url" 2>/dev/null; then
      log "$url -> 200 (after ${waited}s)"
      return 0
    fi
    if ! run_root systemctl is-active --quiet nebulaflow; then
      warn "nebulaflow 进程当前不在 active（首次启动可能在跑内嵌迁移，继续等到超时）"
    fi
    sleep 2
    waited=$((waited + 2))
  done
  warn "=========================================================="
  warn " /healthz 在 ${HEALTH_TIMEOUT}s 内没有返回 200。最后 60 行服务日志："
  run_root journalctl -u nebulaflow -n 60 --no-pager >&2 || true
  warn "=========================================================="
  die "$url 未就绪，中止部署（前端不会切换，线上仍是上一版）"
}
wait_healthz

# 状态回显：方便 CI 日志里直接看到"这台机器上现在跑的是什么"
log "docker compose ps:"
compose ps || true
log "systemctl is-active nebulaflow: $(run_root systemctl is-active nebulaflow 2>/dev/null || true)"
log "binary: $(run_root stat -c '%y  %s bytes' "$BIN_DIR/nebulaflow-server")"

# ---------- 5. 演示数据（每次部署都跑一遍；失败只告警不阻断） ----------
# 为什么必须**每次都跑**：seed 全流程幂等（用户/Provider/知识库/示例工作流/
# 系统模板/系统 Agent 全部按名字复用），而"只在首次跑"会让**之后新增的系统级
# 种子数据永远补不上** —— 线上模板市场为空就是这么来的：首次部署时 seed 把
# 5 个系统模板都写失败（user_id=0 被 workflows 的外键拒绝，SQLSTATE 23503），
# 旧脚本随即打下 .seeded 标记，之后每次部署都直接跳过，故障被永久固化。
# 代价只是一次连库 + 若干 SELECT，比"线上少半个功能"便宜得多。
#
# 失败不阻断的理由不变：seed 只影响演示内容；让一次失败把整个部署回退
# （连带前端切不回去）代价远大于收益，而且 /api/auth/register 依然可用。
if [ -f "$SEED_MARKER" ]; then
  log "seed 上次成功于 $(run_root stat -c '%y' "$SEED_MARKER" 2>/dev/null || echo unknown)（仍会重跑，seed 幂等）"
fi
log "执行 nebulaflow-seed（demo/demo123456 + 示例工作流/知识库 + 系统模板 + 系统 Agent）"
if run_root bash -c "set -a; . '$ENV_FILE'; set +a; '$BIN_DIR/nebulaflow-seed'"; then
  # 标记只表示"最近一次成功"，不再用于决定是否执行
  run_root touch "$SEED_MARKER"
  log "seed ok（系统模板已就位，可用 curl -H 'Authorization: Bearer <token>' .../api/templates 复核非空）"
else
  warn "seed 失败 —— 部署继续（演示账号/系统模板/系统 Agent 可能缺失，可用 POST /api/auth/register 注册账号）。
    手动重试：set -a; . $ENV_FILE; set +a; $BIN_DIR/nebulaflow-seed
    注意：新版本 seed 在系统模板或系统 Agent 写不进去时会以非零退出码失败，
    这正是为了不再出现"退出码 0 但线上模板市场是空的"这种静默故障。"
fi

log "server setup ok"
