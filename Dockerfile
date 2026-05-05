# ---- Stage 1: Build ----
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /server ./cmd/server

# ---- Stage 2: Final ----
FROM alpine:3.20

RUN addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /server /app/server

USER app

EXPOSE 8080

ENTRYPOINT ["/app/server"]
