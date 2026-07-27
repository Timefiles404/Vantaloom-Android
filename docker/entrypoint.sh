#!/bin/sh
# ============================================================================
# Vantaloom 容器启动脚本
#
# 做四件事，然后把自己 exec 掉（让 vantaloom-api 成为 PID 1，直接收到 docker stop
# 的信号，不需要额外的 init 进程转发）：
#
#   1. 准备数据卷上的目录结构，并确认真的写得进去（写不进就当场报清楚，
#      而不是让运行时在半小时后以某个奇怪的错误崩掉）
#   2. 强制打开多人协同：容器里没有「本机可信」这回事
#   3. 首次启动打印引导信息
#   4. 以 0.0.0.0 起 vantaloom-api
#
# 设计文档：docs/multi-user-collaboration.md §7、docs/docker-deployment.md
# ============================================================================
set -eu

DATA_DIR="${VANTALOOM_DATA_DIR:-/data}"
INSTALL_DIR="${VANTALOOM_INSTALL_DIR:-/app}"
BIND_HOST="${VANTALOOM_BIND_HOST:-0.0.0.0}"
BIND_PORT="${VANTALOOM_BIND_PORT:-8780}"

# ── 逃生口：docker run <image> bash ─────────────────────────────────────────
# 第一个参数不以 - 开头 = 用户想跑别的命令（进容器排障），直接执行它。
# 以 - 开头的参数则透传给 vantaloom-api（例如 --hub-url ...）。
if [ "$#" -gt 0 ] && [ "${1#-}" = "$1" ]; then
  exec "$@"
fi

log() { printf '%s\n' "$*" >&2; }
fail() { log ""; log "!! $*"; log ""; exit 1; }

# ── 1. 数据目录 ─────────────────────────────────────────────────────────────
# config/ 与 logs/ 是 /app 下两个软链的目标，**必须先于运行时存在**：悬空软链会
# 让 Go 的 os.MkdirAll 直接失败（它 Stat 不到目标，又 Mkdir 不出已存在的软链）。
# home/     容器用 HOME=/data/home：git config、ssh key、pip/npm 缓存都落在卷上，
#           换镜像升级不丢。
# workspace/ 建议给 agent 用的工作目录（不强制，纯粹是个约定俗成的落脚点）。
mkdir -p \
  "${DATA_DIR}/config" \
  "${DATA_DIR}/logs" \
  "${DATA_DIR}/home" \
  "${DATA_DIR}/workspace" \
  2>/dev/null || true

if [ ! -d "${DATA_DIR}" ]; then
  fail "数据目录 ${DATA_DIR} 不存在，也建不出来。"
fi

probe="${DATA_DIR}/.write-probe.$$"
if ! (: >"${probe}") 2>/dev/null; then
  log ""
  log "!! 数据目录 ${DATA_DIR} 不可写（容器以 uid=$(id -u) gid=$(id -g) 运行）。"
  log ""
  log "   用具名卷（compose.yml 的默认写法）时不会出现这个问题。"
  log "   如果你换成了绑定挂载（例如 -v ./data:/data），宿主目录的属主是宿主用户，"
  log "   容器里的非 root 用户写不进去。修法二选一："
  log ""
  log "     sudo chown -R 10001:10001 ./data      # 推荐"
  log "     docker run --user 0:0 ...             # 以 root 跑，安全性打折"
  log ""
  exit 1
fi
rm -f "${probe}"

# 补建两个软链的目标之后，再确认软链本身是通的（镜像里建的软链 + 这里建的目标）。
for linked in "${INSTALL_DIR}/config" "${INSTALL_DIR}/logs"; do
  [ -d "${linked}" ] || fail "${linked} 不可用（软链目标缺失？）。这是运行时的写路径，缺了会丢 Hub 登录态与诊断日志。"
done

# ── 2. 强制多人协同 ─────────────────────────────────────────────────────────
# 桌面版能默认「不要求登录」，靠的是唯一那条网络边界：只监听 127.0.0.1。
# 容器必然 bind 0.0.0.0，那条边界不存在——不开身份层就等于把一个**无认证的远程
# shell** 挂到网上（docs/multi-user-collaboration.md §0）。
#
# 这个变量的语义由运行时实现（契约见 docs/docker-deployment.md「运行时契约」一节）。
# 运行时版本太老 = 它根本不认识这个变量 = 起来就是裸奔，所以这里 fail closed。
RUNTIME_VERSION="$(cat "${INSTALL_DIR}/VERSION" 2>/dev/null | tr -d '[:space:]' || true)"
[ -n "${RUNTIME_VERSION}" ] || RUNTIME_VERSION="unknown"
MIN_VERSION="${VANTALOOM_MIN_COLLAB_VERSION:-0.15.16}"

version_lt() {
  # $1 < $2 ?  用 sort -V 做语义化版本比较（coreutils 自带）。
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$1" ]
}

if [ "${VANTALOOM_ALLOW_INSECURE:-0}" = "1" ]; then
  log "!! VANTALOOM_ALLOW_INSECURE=1：跳过身份层强制。"
  log "!! 这个容器的 API 面对任何能连到它的人都是敞开的——只在完全可信的隔离网络里这么做。"
elif [ "${RUNTIME_VERSION}" = "unknown" ]; then
  fail "读不到运行时版本（${INSTALL_DIR}/VERSION），无法确认它是否强制登录。镜像构建有问题，拒绝启动。"
elif version_lt "${RUNTIME_VERSION}" "${MIN_VERSION}"; then
  log ""
  log "!! 拒绝启动：运行时 ${RUNTIME_VERSION} 早于 ${MIN_VERSION}，不认识 VANTALOOM_COLLAB_REQUIRED。"
  log ""
  log "   这个版本的本地 API 在没有本机 loopback 边界时是**零认证**的：任何能访问"
  log "   本容器端口的人都可以读全部对话、读写任意文件、执行任意命令。"
  log "   容器必须 bind 0.0.0.0，所以这里不能放行。"
  log ""
  log "   正确做法：用 --build-arg VERSION=${MIN_VERSION} 或更新的版本重新构建镜像。"
  log "   明知风险仍要跑（例如完全隔离的实验网络）：设 VANTALOOM_ALLOW_INSECURE=1。"
  log ""
  exit 1
else
  VANTALOOM_COLLAB_REQUIRED=1
  export VANTALOOM_COLLAB_REQUIRED
fi

# VANTALOOM_TRUSTED_ORIGIN 由 docker 直接注入环境，运行时自己读；这里只回显，
# 方便反代出问题时一眼看清容器实际收到的是什么。
log "Vantaloom ${RUNTIME_VERSION} · 监听 ${BIND_HOST}:${BIND_PORT} · 数据目录 ${DATA_DIR}"
if [ -n "${VANTALOOM_TRUSTED_ORIGIN:-}" ]; then
  log "可信来源（反代域名）: ${VANTALOOM_TRUSTED_ORIGIN}"
fi
if [ -n "${VANTALOOM_PUBLIC_BASE_URL:-}" ]; then
  # 注意：运行时目前**还没有**消费这个变量（见部署文档的运行时契约表）。
  # 眼下它只影响下面首启动引导里打印的地址。
  log "对外访问地址（当前仅用于启动提示）: ${VANTALOOM_PUBLIC_BASE_URL}"
fi

# ── 3. 首次启动引导 ─────────────────────────────────────────────────────────
# 判据用 collab 状态目录：只要还没建过本地账号，就当作没初始化。
if [ ! -e "${DATA_DIR}/collab/state.json" ]; then
  log ""
  log "  ============================================================"
  log "   首次启动：这台 Vantaloom 还没有任何账号"
  log ""
  log "   1. 用浏览器打开  ${VANTALOOM_PUBLIC_BASE_URL:-http://<服务器地址>:${BIND_PORT}}"
  log "   2. 按向导创建属主账号并设置密码"
  log "   3. 之后在「设置 → 账号」里给同事建子账户"
  log ""
  log "   向导完成之前，除健康检查与登录自举端点外一律拒绝访问。"
  log "  ============================================================"
  log ""
fi

# ── 4. 起服务 ───────────────────────────────────────────────────────────────
# --install-dir 同时决定了 web 静态资源目录（<installDir>/web）和运行时的两处写
# 路径（config/、logs/，见 Dockerfile 里的软链说明）。
exec "${INSTALL_DIR}/bin/vantaloom-api" \
  --host "${BIND_HOST}" \
  --port "${BIND_PORT}" \
  --data-dir "${DATA_DIR}" \
  --install-dir "${INSTALL_DIR}" \
  "$@"
