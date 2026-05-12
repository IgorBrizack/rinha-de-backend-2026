# ---- Stage 0: Download and preprocess reference dataset ----
FROM golang:1.24-alpine AS preprocessor

RUN apk add --no-cache curl

WORKDIR /preprocess

COPY go.mod ./
RUN go mod download

COPY cmd/preprocess ./cmd/preprocess
COPY internal/domain ./internal/domain

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /preprocess/preprocess ./cmd/preprocess

RUN mkdir -p /data

# Download reference dataset (~16 MB compressed) and auxiliary files
RUN curl -fL "https://github.com/zanfranceschi/rinha-de-backend-2026/raw/refs/heads/main/resources/references.json.gz" \
        -o /tmp/references.json.gz && \
    curl -fL "https://github.com/zanfranceschi/rinha-de-backend-2026/raw/refs/heads/main/resources/mcc_risk.json" \
        -o /data/mcc_risk.json

# Quantize, convert to i16 binary format, and sample 100K representative vectors (~2.9 MB output)
RUN /preprocess/preprocess -input /tmp/references.json.gz -output /data/references.bin -max-samples 100000 -ivf-k 256 && \
    rm /tmp/references.json.gz

# ---- Stage 1: Build server ----
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /server ./cmd/server

# ---- Stage 2: Final ----
FROM alpine:3.20

RUN apk add --no-cache curl

RUN addgroup -S app && adduser -S app -G app

RUN mkdir -p /run/sockets && chmod 777 /run/sockets

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=preprocessor /data /app/data

RUN chown -R app:app /app/data

USER app

EXPOSE 8080

ENTRYPOINT ["/app/server"]
