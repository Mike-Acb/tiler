FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/tiler-console .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
COPY --from=build /out/tiler-console /usr/local/bin/tiler-console
USER 10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/tiler-console"]
