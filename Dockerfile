# Build stage.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bargainproxy ./cmd/bargainproxy

# Run stage: distroless-style minimal image with certs + tzdata.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S bp && adduser -S bp -G bp
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=build /out/bargainproxy /app/bargainproxy
RUN mkdir -p /app/data && chown -R bp:bp /app
USER bp
EXPOSE 13140
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:13140/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/bargainproxy"]
CMD ["-config", "/app/config.json"]
