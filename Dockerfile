FROM golang:1.20-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o tiler .

FROM alpine:3.18
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/tiler .
COPY conf.toml .
COPY geojson/ ./geojson/
ENTRYPOINT ["./tiler", "-c", "/app/conf.toml"]
