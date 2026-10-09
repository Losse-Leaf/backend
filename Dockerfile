# ==========================================
# Stage 1: Build Go Backend Service
# ==========================================
FROM golang:1.24-alpine AS builder
WORKDIR /build

RUN apk add --no-cache git ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 编译 CGO-Free 静态二进制
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o loose-leaf-server main.go

# ==========================================
# Stage 2: Minimal Production Image
# ==========================================
FROM alpine:3.21
WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

COPY --from=builder /build/loose-leaf-server /app/loose-leaf-server

RUN mkdir -p /app/data

ENV PORT=8088
ENV DB_PATH=/app/data/chimu.db
ENV GIN_MODE=release

EXPOSE 8088
VOLUME ["/app/data"]

ENTRYPOINT ["/app/loose-leaf-server"]
