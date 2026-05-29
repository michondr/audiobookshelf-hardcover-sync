FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git && \
    go install github.com/a-h/templ/cmd/templ@v0.3.857

WORKDIR /app
COPY . .
RUN templ generate
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/server .
RUN mkdir -p /data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["./server"]
