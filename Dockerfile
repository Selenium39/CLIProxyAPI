# Frontend builder stage
FROM node:20-alpine AS frontend-builder

WORKDIR /frontend

COPY Cli-Proxy-API-Management-Center/package*.json ./
RUN npm ci --silent

COPY Cli-Proxy-API-Management-Center/ ./
RUN npm run build

# Go builder stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

# Copy frontend build to static directory
RUN mkdir -p ./static
COPY --from=frontend-builder /frontend/dist/index.html ./static/management.html

FROM alpine:3.22.0

RUN apk add --no-cache tzdata bash curl libstdc++ libgcc

# Install cursor-agent CLI
ARG CURSOR_AGENT_VERSION=2026.04.29-c83a488
RUN ARCH="$(uname -m)" && \
    case "${ARCH}" in x86_64|amd64) ARCH="x64";; arm64|aarch64) ARCH="arm64";; esac && \
    DOWNLOAD_URL="https://downloads.cursor.com/lab/${CURSOR_AGENT_VERSION}/linux/${ARCH}/agent-cli-package.tar.gz" && \
    mkdir -p /root/.local/share/cursor-agent/versions/${CURSOR_AGENT_VERSION} && \
    curl -fSL "${DOWNLOAD_URL}" | tar --strip-components=1 -xzf - -C /root/.local/share/cursor-agent/versions/${CURSOR_AGENT_VERSION} && \
    mkdir -p /root/.local/bin && \
    ln -sf /root/.local/share/cursor-agent/versions/${CURSOR_AGENT_VERSION}/cursor-agent /root/.local/bin/cursor-agent

ENV PATH="/root/.local/bin:${PATH}"

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY --from=builder ./app/static /CLIProxyAPI/static

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
