# An Ubuntu 24.04 template modeled on a boat.dev machine: the default template's tools and
# desktop, plus Rust, Ruby, PHP, Java, Kotlin, Scala, .NET, Elixir, R, Bun, and Deno.
# Node.js, Go, uv, gh, cua-driver, the agent CLIs, and the desktop's scripts are copied from
# the default template's image, so both templates pin them in one place. Build `default`
# first; `sbx template rm default` also removes the image this build copies from.
FROM localhost/sbx-template/default:latest AS default

FROM ubuntu:24.04

ARG RUST_VERSION=1.97.1
ARG RUSTUP_VERSION=1.28.2
ARG RUBY_VERSION=4.0.1
ARG RUBY_SHA256=3924be2d05db30f4e35f859bf028be85f4b7dd01714142fd823e4af5de2faf9d
ARG BUN_VERSION=1.3.14
ARG DENO_VERSION=2.9.4
ARG COMPOSER_VERSION=2.10.2
ARG POETRY_VERSION=2.4.1
ARG KOTLIN_VERSION=2.1.0
ARG GRADLE_VERSION=8.12
ARG SCALA_VERSION=3.3.8
ARG SBT_VERSION=1.13.0
ARG OPENCODE_VERSION=1.19.5
ARG PI_VERSION=1.5.0

ENV DEBIAN_FRONTEND=noninteractive \
    RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:/usr/local/go/bin:$PATH

# PHP 8.4 comes from the ondrej PPA and R 4.6 from CRAN; Ubuntu ships PHP 8.3 and R 4.3.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && curl -fsSL 'https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x14AA40EC0831756756D7F66C4F4EA0AAE5267A6C' \
      | gpg --dearmor -o /usr/share/keyrings/ondrej-php.gpg \
    && echo 'deb [signed-by=/usr/share/keyrings/ondrej-php.gpg] https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble main' \
      >/etc/apt/sources.list.d/ondrej-php.list \
    && curl -fsSL https://cloud.r-project.org/bin/linux/ubuntu/marutter_pubkey.asc \
      | gpg --dearmor -o /usr/share/keyrings/cran.gpg \
    && echo 'deb [signed-by=/usr/share/keyrings/cran.gpg] https://cloud.r-project.org/bin/linux/ubuntu noble-cran40/' \
      >/etc/apt/sources.list.d/cran.list \
    && curl -fsSLo /tmp/chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
    && apt-get update && apt-get install -y --no-install-recommends \
      /tmp/chrome.deb \
      wget git git-lfs openssh-client sudo less vim nano \
      bash-completion procps psmisc iproute2 iputils-ping dnsutils netcat-openbsd lsof htop \
      strace tcpdump mtr-tiny net-tools tmux screen time \
      build-essential pkg-config cmake ninja-build autoconf patch patchelf clang llvm \
      libssl-dev zlib1g-dev libbz2-dev libreadline-dev libsqlite3-dev libffi-dev liblzma-dev \
      libncurses-dev libyaml-dev libxml2-dev libgmp-dev tk-dev \
      python3 python3-venv python3-pip python3-dev \
      jq ripgrep fd-find tree unzip zip xz-utils zstd pigz file rsync sqlite3 \
      ffmpeg imagemagick \
      fonts-noto-cjk fonts-noto-color-emoji fonts-liberation fonts-dejavu-core \
      tigervnc-standalone-server openbox x11-xserver-utils xfce4-terminal thunar tint2 feh adwaita-icon-theme \
      xdotool wmctrl xclip scrot tesseract-ocr tesseract-ocr-eng tesseract-ocr-chi-tra tesseract-ocr-jpn \
      at-spi2-core python3-pyatspi dbus-x11 \
      fcitx5 fcitx5-frontend-gtk3 fcitx5-mozc fcitx5-chinese-addons fcitx5-chewing \
      openjdk-11-jdk maven \
      php8.4-cli php8.4-bcmath php8.4-curl php8.4-gd php8.4-intl php8.4-mbstring php8.4-mysql \
      php8.4-opcache php8.4-readline php8.4-xml php8.4-zip \
      dotnet-sdk-8.0 erlang elixir r-base r-base-dev \
      locales tzdata \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && ln -s /usr/bin/google-chrome-stable /usr/bin/chromium \
    && locale-gen en_US.UTF-8 ja_JP.UTF-8 zh_TW.UTF-8 \
    && rm -rf /var/lib/apt/lists/* /tmp/chrome.deb

COPY --from=default /usr/local/go /usr/local/go
COPY --from=default /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=default /usr/local/include/node /usr/local/include/node
COPY --from=default /usr/local/bin/ /usr/local/bin/

RUN set -eux; cd /tmp; \
    curl -fsSLO https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-gnu/rustup-init; \
    echo "$(curl -fsSL https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-gnu/rustup-init.sha256 | cut -d' ' -f1)  rustup-init" | sha256sum -c -; \
    chmod +x rustup-init; \
    ./rustup-init -y --no-modify-path --profile minimal --default-toolchain $RUST_VERSION \
      --component clippy,rustfmt,rust-analyzer; \
    ruby=ruby-$RUBY_VERSION.tar.gz; \
    curl -fsSLO https://cache.ruby-lang.org/pub/ruby/${RUBY_VERSION%.*}/$ruby; \
    echo "$RUBY_SHA256  $ruby" | sha256sum -c -; \
    tar -xzf $ruby; \
    (cd ruby-$RUBY_VERSION && ./configure --prefix=/usr/local --disable-install-doc && make -j"$(nproc)" && make install); \
    bun=bun-linux-x64.zip; \
    curl -fsSLO https://github.com/oven-sh/bun/releases/download/bun-v$BUN_VERSION/$bun; \
    curl -fsSL https://github.com/oven-sh/bun/releases/download/bun-v$BUN_VERSION/SHASUMS256.txt | grep " $bun\$" | sha256sum -c -; \
    unzip -j $bun bun-linux-x64/bun -d /usr/local/bin; \
    ln -s bun /usr/local/bin/bunx; \
    deno=deno-x86_64-unknown-linux-gnu.zip; \
    curl -fsSLO https://github.com/denoland/deno/releases/download/v$DENO_VERSION/$deno; \
    curl -fsSL https://github.com/denoland/deno/releases/download/v$DENO_VERSION/$deno.sha256sum | sha256sum -c -; \
    unzip $deno deno -d /usr/local/bin; \
    curl -fsSLo /usr/local/bin/composer https://getcomposer.org/download/$COMPOSER_VERSION/composer.phar; \
    echo "$(curl -fsSL https://getcomposer.org/download/$COMPOSER_VERSION/composer.phar.sha256sum | cut -d' ' -f1)  /usr/local/bin/composer" | sha256sum -c -; \
    chmod +x /usr/local/bin/composer; \
    kotlin=kotlin-compiler-$KOTLIN_VERSION.zip; \
    curl -fsSLO https://github.com/JetBrains/kotlin/releases/download/v$KOTLIN_VERSION/$kotlin; \
    echo "$(curl -fsSL https://github.com/JetBrains/kotlin/releases/download/v$KOTLIN_VERSION/$kotlin.sha256)  $kotlin" | sha256sum -c -; \
    unzip -q $kotlin -d /opt; \
    gradle=gradle-$GRADLE_VERSION-bin.zip; \
    curl -fsSLO https://services.gradle.org/distributions/$gradle; \
    echo "$(curl -fsSL https://services.gradle.org/distributions/$gradle.sha256)  $gradle" | sha256sum -c -; \
    unzip -q $gradle -d /opt; \
    mv /opt/gradle-$GRADLE_VERSION /opt/gradle; \
    scala=scala3-$SCALA_VERSION.tar.gz; \
    curl -fsSLO https://github.com/scala/scala3/releases/download/$SCALA_VERSION/$scala; \
    curl -fsSL https://github.com/scala/scala3/releases/download/$SCALA_VERSION/sha256sum.txt | grep "/$scala\$" | sed 's|dist/target/||' | sha256sum -c -; \
    tar -xzf $scala -C /opt; \
    mv /opt/scala3-$SCALA_VERSION /opt/scala3; \
    sbt=sbt-$SBT_VERSION.tgz; \
    curl -fsSLO https://github.com/sbt/sbt/releases/download/v$SBT_VERSION/$sbt; \
    curl -fsSL https://github.com/sbt/sbt/releases/download/v$SBT_VERSION/$sbt.sha256 | sha256sum -c -; \
    tar -xzf $sbt -C /opt; \
    rm -rf /tmp/*; \
    UV_TOOL_DIR=/opt/uv-tools UV_TOOL_BIN_DIR=/usr/local/bin uv tool install poetry==$POETRY_VERSION; \
    npm install -g --no-audit --no-fund opencode-ai@$OPENCODE_VERSION @earendil-works/pi-coding-agent@$PI_VERSION; \
    rm -rf /root/.npm /root/.cache

# The ubuntu image has its own uid 1000; sandboxes run commands as `user` with that uid.
# `user` owns the Rust toolchain so `cargo install` and `rustup` work without sudo.
RUN userdel -r ubuntu \
    && useradd -m -u 1000 -s /bin/bash user \
    && echo 'user ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/user && chmod 0440 /etc/sudoers.d/user \
    && chown -R user:user /usr/local/rustup /usr/local/cargo \
    && runuser -u user -- cua-driver telemetry disable

COPY --from=default /etc/profile.d/sbx.sh /etc/profile.d/sbx.sh
COPY --from=default /etc/xdg/openbox/menu.xml /etc/xdg/openbox/menu.xml
COPY --from=default /etc/xdg/fcitx5/profile /etc/xdg/fcitx5/config /etc/xdg/fcitx5/
COPY --from=default /usr/local/share/sbx/ /usr/local/share/sbx/
COPY profile.sh /etc/profile.d/sbx-ubuntu.sh
RUN sed -i 's/^Icon=chromium$/Icon=google-chrome/' /usr/local/share/sbx/launchers/chromium.desktop
