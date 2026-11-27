# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS go-builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=3.16.7
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X agy-gateway/internal/version.Version=${VERSION}" \
    -o /out/agy-gateway ./cmd/agy-gateway && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X agy-gateway/internal/version.Version=${VERSION}" \
    -o /out/agy-accounts ./cmd/agy-accounts

FROM debian:bookworm-slim AS runtime
ARG TARGETARCH=amd64
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      curl \
      tar \
      gzip \
      nodejs \
      npm \
      python3 \
      python3-pip \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @fikrilal/agy-auth \
    && curl -fsSL https://antigravity.google/cli/install.sh | bash \
    && cp -f /root/.local/bin/agy /usr/local/bin/agy \
    && rm -rf /root/.local /root/.cache \
    && groupadd --gid 10001 agygw \
    && useradd --uid 10001 --gid 10001 --shell /usr/sbin/nologin --create-home agygw \
    && mkdir -p /etc/agy-gateway \
                /var/lib/agy-gateway/bin \
                /var/lib/agy-gateway/vault-xdg \
                /var/lib/agy-gateway/workers \
                /var/lib/agy-gateway/tmp \
                /var/lib/agy-gateway/workspaces \
    && chown -R 10001:10001 /etc/agy-gateway /var/lib/agy-gateway

COPY --from=go-builder /out/agy-gateway /usr/local/bin/agy-gateway
COPY --from=go-builder /out/agy-accounts /usr/local/bin/agy-accounts
COPY config.yaml /etc/agy-gateway/config.yaml
RUN chmod 0755 /usr/local/bin/agy-gateway /usr/local/bin/agy-accounts /usr/local/bin/agy && \
    chown -R 10001:10001 /etc/agy-gateway /var/lib/agy-gateway

ENV PATH="/var/lib/agy-gateway/bin:/home/agygw/.local/bin:/usr/local/bin:$PATH" \
    DBUS_SESSION_BUS_ADDRESS="" \
    AGY_AUTH_NO_UPDATE_CHECK="1" \
    XDG_CONFIG_HOME="/var/lib/agy-gateway/vault-xdg" \
    NODE_USE_ENV_PROXY="1"

USER 10001:10001
EXPOSE 8080
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=10s \
  CMD ["/usr/local/bin/agy-gateway", "-config", "/etc/agy-gateway/config.yaml", "-healthcheck"]

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/agy-gateway"]
CMD ["--config", "/etc/agy-gateway/config.yaml"]
