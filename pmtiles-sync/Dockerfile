FROM golang:1.25-alpine AS go-builder
RUN go install github.com/protomaps/go-pmtiles@latest
# dl.min.io 已不再提供 mc 二进制下载（410 Gone），改为从源码编译；版本锁定，升级时改这里
RUN CGO_ENABLED=0 go install github.com/minio/mc@v0.0.0-20251106162529-77f82e18b540

FROM python:3.11-slim

COPY --from=go-builder /go/bin/go-pmtiles /usr/local/bin/pmtiles
COPY --from=go-builder /go/bin/mc /usr/local/bin/mc

COPY sync.sh detect-format.sh dir2mbtiles.py apply-metadata.sh /app/
RUN chmod +x /app/sync.sh /app/detect-format.sh /app/dir2mbtiles.py /app/apply-metadata.sh

ENTRYPOINT ["/app/sync.sh"]
