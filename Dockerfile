# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
USER app
WORKDIR /app
COPY --from=build /out/wb2api /app/wb2api
COPY config.example.json /app/config.json
EXPOSE 7863
# 健康检查端口：从 config.json 的 listen 字段实时解析，避免写死端口。
#
# 为什么不能写死：网关支持自定义监听端口（如本机用 7864 与旧实例并存），
# 而 HEALTHCHECK 是镜像构建期固化的。写死 7863 会导致换端口部署时探活永远失败
# （容器显示 unhealthy，但服务其实正常）——这会误导编排/负载均衡摘流量。
# 这里用 sed 从 /app/config.json 提取 listen 的端口号；解析失败则回落 7863。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD sh -c 'p=$(sed -n "s/.*\"listen\"[[:space:]]*:[[:space:]]*\"[^\"]*:\([0-9]\+\).*/\1/p" /app/config.json | head -1); [ -z "$p" ] && p=7863; wget -qO- "http://127.0.0.1:$p/healthz" || exit 1'
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
