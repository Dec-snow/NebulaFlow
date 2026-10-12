# ---- Stage 1: Frontend build ----
FROM node:22-alpine AS frontend

WORKDIR /web
COPY web-next/package.json web-next/package-lock.json ./
RUN npm ci

COPY web-next/ .
RUN npm run build

# ---- Stage 2: Go build ----
FROM golang:1.27-alpine AS builder

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy Go source (separate layer — only invalidates on Go code changes)
COPY internal/ internal/
COPY cmd/ cmd/

# Copy frontend build artifacts (separate layer — only invalidates on frontend changes)
COPY --from=frontend /web/dist ./web-next/dist

# Build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o /out/nebula-server ./cmd/server

# ---- Stage 3: Runtime ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 -g 1000 nebula

WORKDIR /app
COPY --from=builder /out/nebula-server /app/nebula-server
COPY --from=builder /src/web-next/dist /app/web-next/dist

USER nebula

EXPOSE 8080

CMD ["/app/nebula-server"]
