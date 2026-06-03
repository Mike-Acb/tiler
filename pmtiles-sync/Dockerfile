FROM golang:1.25-alpine AS pmtiles-builder
RUN go install github.com/protomaps/go-pmtiles@latest

FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends curl && \
    rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir mbutil

COPY --from=pmtiles-builder /go/bin/go-pmtiles /usr/local/bin/pmtiles

RUN curl -fsSL https://dl.min.io/client/mc/release/linux-amd64/mc \
    -o /usr/local/bin/mc && chmod +x /usr/local/bin/mc

COPY sync.sh /app/sync.sh
RUN chmod +x /app/sync.sh

ENTRYPOINT ["/app/sync.sh"]
