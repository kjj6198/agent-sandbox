# The default sbx template: a Debian machine with the toolchains coding agents reach for.
# sbxd adds its agent as init when it turns the image into a disk, so there is no ENTRYPOINT.
FROM debian:trixie-slim

ARG NODE_VERSION=24.21.0
ARG GO_VERSION=1.27.2
ARG UV_VERSION=0.13.0
ARG PNPM_VERSION=12.11.2

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git git-lfs openssh-client gnupg sudo less vim-tiny nano \
      bash-completion procps psmisc iproute2 iputils-ping dnsutils netcat-openbsd lsof htop \
      build-essential pkg-config cmake \
      python3 python3-venv python3-pip python3-dev \
      jq ripgrep fd-find tree unzip zip xz-utils zstd file rsync sqlite3 \
      ffmpeg imagemagick \
      chromium fonts-noto-cjk fonts-noto-color-emoji fonts-liberation fonts-dejavu-core \
      tigervnc-standalone-server openbox x11-xserver-utils xterm \
      locales tzdata \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && sed -i -E 's/^# (en_US|ja_JP|zh_TW)\.UTF-8/\1.UTF-8/' /etc/locale.gen && locale-gen \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; cd /tmp; \
    node=node-v$NODE_VERSION-linux-x64.tar.xz; \
    curl -fsSLO https://nodejs.org/dist/v$NODE_VERSION/$node; \
    curl -fsSL https://nodejs.org/dist/v$NODE_VERSION/SHASUMS256.txt | grep " $node\$" | sha256sum -c -; \
    tar -xJf $node -C /usr/local --strip-components=1 --exclude='*.md' --exclude=LICENSE; \
    go=go$GO_VERSION.linux-amd64.tar.gz; \
    curl -fsSLO https://dl.google.com/go/$go; \
    echo "$(curl -fsSL https://dl.google.com/go/$go.sha256)  $go" | sha256sum -c -; \
    tar -xzf $go -C /usr/local; \
    uv=uv-x86_64-unknown-linux-gnu.tar.gz; \
    curl -fsSLO https://github.com/astral-sh/uv/releases/download/$UV_VERSION/$uv; \
    curl -fsSL https://github.com/astral-sh/uv/releases/download/$UV_VERSION/$uv.sha256 | sha256sum -c -; \
    tar -xzf $uv -C /usr/local/bin --strip-components=1; \
    rm -f /tmp/*.tar.*; \
    npm install -g --no-audit --no-fund pnpm@$PNPM_VERSION; \
    rm -rf /root/.npm

RUN useradd -m -u 1000 -s /bin/bash user \
    && echo 'user ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/user && chmod 0440 /etc/sudoers.d/user

COPY profile.sh /etc/profile.d/sbx.sh
COPY sbx-desktop sbx-browser /usr/local/bin/
COPY openbox-menu.xml /etc/xdg/openbox/menu.xml
COPY wallpaper.xbm /usr/local/share/sbx/
