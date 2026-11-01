FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/news-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 newsproxy \
 && mkdir /data && chown newsproxy /data
COPY --from=build /out/news-proxy /usr/local/bin/news-proxy
USER newsproxy
ENV DATA_DIR=/data PORT=7999
VOLUME /data
EXPOSE 7999
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:${PORT}/health >/dev/null || exit 1
ENTRYPOINT ["news-proxy", "run"]
