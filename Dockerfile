# ---- Stage 1: Frontend build ----
FROM node:20-alpine AS frontend

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
RUN go mod download

# Build
COPY . .
COPY --from=frontend /web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/nebula-server ./cmd/server

# ---- Stage 3: Runtime ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 -g 1000 nebula

WORKDIR /app
COPY --from=builder /out/nebula-server /app/nebula-server
COPY --from=builder /src/web/dist /app/web/dist

USER nebula

EXPOSE 8080

CMD ["/app/nebula-server"]
