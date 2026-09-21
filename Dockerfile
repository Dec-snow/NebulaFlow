# NebulaFlow 后端多阶段构建
FROM golang:1.27-alpine AS builder
WORKDIR /app

# 依赖缓存层
COPY go.mod go.sum ./
RUN go mod download

# 源码构建
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/nebulaflow ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/nebulaflow-seed ./cmd/seed

# 前端构建（Node 18 基础镜像）
FROM node:20-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY web/ .
RUN npm run build

# 运行镜像
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=builder /out/nebulaflow /usr/local/bin/nebulaflow
COPY --from=builder /out/nebulaflow-seed /usr/local/bin/nebulaflow-seed
# 前端产物随镜像分发（后端单端口托管）
COPY --from=web /web/dist /app/web/dist
EXPOSE 8080
ENTRYPOINT ["nebulaflow"]
