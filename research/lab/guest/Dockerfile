FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash coreutils procps iproute2 iputils-ping curl ca-certificates socat tini util-linux \
      python3 nodejs npm git build-essential sysbench fio iperf3 \
      chromium fonts-liberation fonts-dejavu-core fonts-noto-cjk xvfb unzip \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 1000 -s /bin/bash agent && mkdir -p /overlay /work

WORKDIR /opt/work
RUN mkdir /tmp/npm && cd /tmp/npm && npm init -y >/dev/null \
    && npm install --no-audit --no-fund --loglevel=error \
         typescript react react-dom eslint webpack webpack-cli @babel/core @babel/preset-env jest \
         @mui/material @emotion/react @emotion/styled rxjs lodash-es \
    && tar cf /opt/work/many-files.tar node_modules \
    && cd / && rm -rf /tmp/npm /root/.npm

# sqlite amalgamation: single-file gcc -O2 compile for the CPU-bound compile test.
RUN curl -fsSLo /tmp/sqlite.zip https://www.sqlite.org/2026/sqlite-amalgamation-3540000.zip \
    && unzip -q /tmp/sqlite.zip -d /tmp && mv /tmp/sqlite-amalgamation-* /opt/work/sqlite && rm /tmp/sqlite.zip

RUN git clone --quiet --depth 500 https://github.com/redis/redis.git /opt/work/redis.git

COPY www /opt/work/www
COPY sbx-init /sbin/sbx-init
COPY sbx-browser /usr/local/bin/sbx-browser
RUN chmod 0755 /sbin/sbx-init /usr/local/bin/sbx-browser && chmod -R a+rX,go-w /opt/work

ENTRYPOINT ["/sbin/sbx-init"]
