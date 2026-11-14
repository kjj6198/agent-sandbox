# sbxd in a container, for a Linux host with /dev/kvm and Docker. compose.yaml runs it.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN make dist VERSION=$VERSION

# Only the kernel config and the installer reach this stage, so Go changes keep its cache.
FROM debian:trixie-slim AS kernel
COPY deploy/install.sh /dist/
COPY kernel /dist/kernel
RUN /dist/install.sh kernel

FROM debian:trixie-slim
RUN --mount=type=bind,from=build,source=/src/dist,target=/dist \
    apt-get update -q \
    && apt-get install -y -q --no-install-recommends tini \
    && /dist/install.sh packages firecracker sbx \
    && install -m 0755 /dist/install.sh /usr/local/lib/sbx/install.sh \
    && rm -rf /var/lib/apt/lists/*
COPY --from=kernel /var/lib/sbx/kernel /usr/local/lib/sbx/kernel
COPY deploy/docker/entrypoint.sh deploy/docker/sbxd.json /usr/local/lib/sbx/
VOLUME ["/var/lib/sbx", "/etc/sbx", "/var/lib/containers"]
EXPOSE 7070 30000-30099
ENTRYPOINT ["tini", "--", "/usr/local/lib/sbx/entrypoint.sh"]
CMD ["sbxd", "--config", "/usr/local/lib/sbx/sbxd.json"]
