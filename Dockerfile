# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/nebula-server ./cmd/server

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 -g 1000 nebula

WORKDIR /app
COPY --from=builder /out/nebula-server /app/nebula-server

USER nebula

EXPOSE 8080

CMD ["/app/nebula-server"]
