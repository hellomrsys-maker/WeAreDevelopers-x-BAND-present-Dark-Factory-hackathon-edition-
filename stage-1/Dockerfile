# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/server ./cmd/server

# Clean runtime container (offline, zero outbound network)
FROM alpine:3.21

RUN apk add --no-cache tzdata ca-certificates

WORKDIR /app
COPY --from=builder /bin/server /app/server

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/app/server"]
