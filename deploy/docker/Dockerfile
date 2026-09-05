# docker build -f deploy/docker/Dockerfile -t athanor:local .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/athanor ./cmd/athanor

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 athanor \
 && mkdir -p /data /data/spool && chown -R athanor /data
COPY --from=build /out/athanor /usr/local/bin/athanor
VOLUME /data
USER athanor
ENV ATHANOR_DB=/data/brain.db \
    ATHANOR_SPOOL=/data/spool \
    ATHANOR_GRPC_ADDR=0.0.0.0:47831 \
    ATHANOR_HTTP_ADDR=0.0.0.0:47832
EXPOSE 47831 47832
# The binary is its own probe: it reads ATHANOR_GRPC_ADDR and ATHANOR_TOKEN
# from the same environment, and probes loopback for a wildcard bind.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["athanor", "-health"]
ENTRYPOINT ["athanor"]
