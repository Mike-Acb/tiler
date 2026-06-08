FROM golang:1.22-alpine AS builder
RUN apk add --no-cache git
RUN go install github.com/ginuerzh/gost/cmd/gost@v2.11.5

FROM alpine:3.18
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /go/bin/gost /usr/local/bin/gost
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
