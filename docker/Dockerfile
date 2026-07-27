# syntax=docker/dockerfile:1.7
#
# Vantaloom 团队版容器镜像
# ============================================================================
# 这个文件（连同 entrypoint.sh / compose.yml / ci/build-docker.yml）会被原样复制
# 进公开仓 Vantaloom/Vantaloom-app 的 docker/ 目录。
#
# 它不包含、也永远不会包含任何 Vantaloom 源码：镜像的**唯一输入是已发布到 npm
# 的编译产物** @vantaloom/runtime-linux-<arch>。这与安卓的 libvantaloom.so、电脑
# 被控端的 zip 是同一条纪律——编译产物进公开仓/公开镜像，源码绝不。
# 参见 docs/multi-user-collaboration.md §7.1。
#
# 构建：
#   docker build -t vantaloom:0.15.16 --build-arg VERSION=0.15.16 docker/
#
# VERSION 必须是 npm 上真实存在的运行时版本，且不低于 entrypoint 里的
# VANTALOOM_MIN_COLLAB_VERSION（更早的运行时不认识强制登录的开关，容器会拒绝
# 启动）。正式发布请钉死版本号：用 latest 会让同一个镜像 tag 在不同时间构建出
# 不同的内容。
# ============================================================================

ARG VERSION=latest
ARG NPM_REGISTRY=https://registry.npmjs.org/

# ─────────────────────────────────────────────────────────────────────────────
# 阶段一 · 取运行时产物
#
# --platform=$BUILDPLATFORM：这一阶段只做「下载 + 解包」，让它跑在构建机的原生
# 架构上，跨架构构建时不必经 QEMU 模拟（慢十倍且偶发段错误）。要拉哪个架构的包
# 由 TARGETARCH 决定，与本阶段自身跑在什么架构上无关。
# ─────────────────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS fetch

ARG VERSION
ARG NPM_REGISTRY
ARG TARGETARCH

WORKDIR /pack

# npm 包的产物布局由 packages/cli 的 writeRuntimePackageMetadata 决定：
#   package/{bin,web,cli,manifest.json,VERSION,README.md}
#
# 下面这条 RUN 做四件事：
#
#  1. 按 TARGETARCH 选包并 npm pack + 解包；
#  2. 断言 bin/vantaloom-api 与 web/index.html 真的在——版本号打错、包结构变了
#     都要在构建期炸掉，而不是等到容器启动时才发现；
#  3. 无条件 chmod +x：**刻意不信任 tar 里的可执行位**。linux 运行时包如果是在
#     Windows 上交叉编译并从 Windows 发布的，打包时可执行位会丢成 0644（公开仓
#     build-node-release.yml 为完全相同的坑专门重打过包）。构建期一行命令，换掉
#     一整类「permission denied」的线上事故；
#  4. 删掉 tgz。
#
# 刻意不取 cli/：那是 Node 写的安装/进程管理器，容器里既没有 Node 也不需要它
# （容器生命周期归 Docker 管，升级 = 换镜像 tag，而不是跑 vantaloom update）。
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) pkg_arch=x64 ;; \
      arm64) pkg_arch=arm64 ;; \
      *) echo "不支持的 TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    pkg="@vantaloom/runtime-linux-${pkg_arch}@${VERSION}"; \
    echo "==> npm pack ${pkg} (registry=${NPM_REGISTRY})"; \
    npm pack "${pkg}" --registry "${NPM_REGISTRY}"; \
    tar -xzf ./*.tgz; \
    test -f package/bin/vantaloom-api || { echo "npm 包里没有 bin/vantaloom-api" >&2; exit 1; }; \
    test -f package/web/index.html || { echo "npm 包里没有 web/index.html" >&2; exit 1; }; \
    chmod +x package/bin/*; \
    rm -f ./*.tgz

# ─────────────────────────────────────────────────────────────────────────────
# 阶段二 · 运行
#
# 基础镜像选 debian:bookworm-slim，不是 alpine。核实过程与结论：
#
#   packages/cli/src/lib/platform.mjs 的 platformToGoEnv() 恒返回 CGO_ENABLED=0，
#   而 packages/cli/src/lib/package.mjs 的 buildRuntimePackage() 对三个二进制都传了
#   目标平台（platform = options.target ?? platformId()，永不为空）→ 每次构建都走
#   这条 env。也就是说 bin/ 里是**纯静态 Go 二进制，不链接 glibc**，单看二进制本身
#   放进 alpine/musl 也能跑。
#
#   仍然选 Debian 的理由与 libc 无关，而是这个容器同时是 agent 的工作环境：agent
#   会在里面跑 shell、git、装工具链，而上游预编译产物（nodejs.org 的 Node、
#   manylinux 的 pip wheel、几乎所有 .deb）都是 glibc 目标，在 musl 上会以各种难查
#   的方式失败。多几十 MB 换 agent 真的能干活，这笔账是划算的。
# ─────────────────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim AS runtime

ARG VERSION

LABEL org.opencontainers.image.title="Vantaloom" \
      org.opencontainers.image.description="Vantaloom 团队版：一台服务器上多人共用的 agent 工作台" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/Vantaloom/Vantaloom-app" \
      org.opencontainers.image.licenses="proprietary"

# 只装运行与 agent 干活的最小集合，不装任何桌面/GUI 相关的东西：
#   ca-certificates  访问模型服务商必需（没有它所有 HTTPS 请求都失败）
#   git              worktree 隔离、仓库操作，agent 的常规工具
#   curl             HEALTHCHECK 用；顺带让人进容器排障时有个趁手的工具
#   tzdata           让 TZ 环境变量真的生效（日志/时间线的本地时间）
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      git \
      curl \
      tzdata; \
    rm -rf /var/lib/apt/lists/*

# 非 root 运行。UID/GID 钉死成 10001，这样绑定挂载的宿主目录可以提前
# `chown -R 10001:10001`（见 docs/docker-deployment.md 的数据卷一节）。
RUN set -eux; \
    groupadd --gid 10001 vantaloom; \
    useradd --uid 10001 --gid 10001 --home-dir /data/home --shell /bin/bash vantaloom

COPY --from=fetch /pack/package/bin           /app/bin
COPY --from=fetch /pack/package/web           /app/web
COPY --from=fetch /pack/package/VERSION       /app/VERSION
COPY --from=fetch /pack/package/manifest.json /app/manifest.json

# ── 为什么 /app/config 与 /app/logs 是指向 /data 的软链 ──────────────────────
# --install-dir 不只是「web 静态资源在哪」，它同时是运行时的**写路径**：
#   · <installDir>/config/local.json —— Hub 登录后的连接配置（server/hub_config.go
#     的 resolveHubConfigFilePath：InstallDir 非空时优先落这里，而不是 dataDir）
#   · <installDir>/logs/requests-*.jsonl —— 请求 dump（server.go 的 reqlog 落盘目录）
# 而 /app 在容器可写层上：不做转接的话，Hub 登录状态与诊断日志会在
# `docker compose down && up` 或换镜像 tag 的那一刻**静默消失**。
# 软链把这两处写入引到数据卷上，升级镜像不丢登录态。
#
# 顺序上必须**先建目录再建软链**：悬空软链会让下面的 chown 报 "cannot dereference"
# 直接中断构建，运行时的 os.MkdirAll 也会栽在同一处。
# 顺带把 home/ 与 workspace/ 一起建出来：具名卷首次挂载时 Docker 会用镜像里该路径
# 的内容做初始化，连属主一起带过去——于是具名卷天生就是可写的。
# （绑定挂载不会被初始化，所以 entrypoint 里还有一次 mkdir + 可写性自检。）
# chown 用 -h：只改软链本身，不去动它指向的东西。
RUN set -eux; \
    mkdir -p /data/config /data/logs /data/home /data/workspace; \
    ln -s /data/config /app/config; \
    ln -s /data/logs   /app/logs; \
    chown -Rh vantaloom:vantaloom /app /data

COPY entrypoint.sh /entrypoint.sh
# CRLF 与可执行位一起处理掉：仓库在 Windows 上开发（core.autocrlf=true），
# 带 \r 的 shebang 会让容器以 "no such file or directory" 这种毫无线索的方式启动
# 失败。docker/.gitattributes 已经钉了 eol=lf，这里是第二道保险。
RUN set -eux; \
    sed -i 's/\r$//' /entrypoint.sh; \
    chmod +x /entrypoint.sh

ENV VANTALOOM_DATA_DIR=/data \
    VANTALOOM_INSTALL_DIR=/app \
    VANTALOOM_BIND_HOST=0.0.0.0 \
    VANTALOOM_BIND_PORT=8780 \
    HOME=/data/home \
    PATH=/app/bin:$PATH

VOLUME ["/data"]
EXPOSE 8780

# vantaloom-api 的 signal.NotifyContext 只登记了 os.Interrupt（SIGINT），**没有
# SIGTERM**。而 docker stop 默认发 SIGTERM——不改这里的话，每次停容器都是等满
# 超时后被 SIGKILL 硬杀，run 中途的落盘没有机会收尾。
# STOPSIGNAL 改成 SIGINT 即可让 docker stop 走正常的优雅退出路径。
# （后端若哪天把 SIGTERM 也登记上，这一行留着依然正确。）
STOPSIGNAL SIGINT

# /v1/health 是协同开启后仍然免认证的白名单端点（server/collab_gate.go 的
# collabPublicPath），所以健康检查不需要任何凭据。
# start-period 给足 20s：首次启动要建数据目录、装载插件、扫描内置插件。
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${VANTALOOM_BIND_PORT:-8780}/v1/health" >/dev/null || exit 1

USER vantaloom
WORKDIR /data

ENTRYPOINT ["/entrypoint.sh"]
