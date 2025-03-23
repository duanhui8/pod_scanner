FROM golang:1.20 as builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o scanner cmd/main.go

FROM alpine:3.21.3
RUN apk add --no-cache curl jq
COPY --from=builder /app/scanner /app/scanner
CMD ["/app/scanner"]