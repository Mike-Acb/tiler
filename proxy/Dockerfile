FROM alpine:3.18
RUN apk add --no-cache ca-certificates curl && \
    curl -fsSL https://github.com/ginuerzh/gost/releases/download/v2.11.5/gost-linux-amd64-2.11.5.gz \
    | gunzip > /usr/local/bin/gost && \
    chmod +x /usr/local/bin/gost
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
