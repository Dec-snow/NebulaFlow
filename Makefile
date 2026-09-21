# NebulaFlow 常用命令
.PHONY: up down logs build test vet run seed bench secret check-config

up:        ## 一键启动全部服务（生产形态：必须先 export JWT_SECRET，见 make secret）
	docker compose up -d --build

down:      ## 停止并清理
	docker compose down

secret:    ## 生成一个可用于生产的 JWT 密钥（32 字节随机）
	@openssl rand -hex 32

check-config: ## 校验当前环境变量下的配置并退出（不连接数据库/Redis）
	go run ./cmd/server -check-config

logs:      ## 查看后端日志
	docker compose logs -f app

build:     ## 本地编译
	go build -o bin/nebulaflow ./cmd/server

run:       ## 本地运行（需 postgres/redis 已启动）
	go run ./cmd/server

test:      ## 全部单元测试
	go test -race -cover ./...

vet:       ## 静态检查
	go vet ./...

seed:      ## 初始化演示数据（mock provider + 示例工作流）
	go run ./cmd/seed

bench:     ## 压测：默认 1000 请求 / 50 并发
	wrk -t8 -c50 -d10s -s benchmark/submit.lua http://localhost:8080/api/tasks

frontend:  ## 前端开发服务器
	cd web && npm run dev

frontend-build:
	cd web && npm ci && npm run build
