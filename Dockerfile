FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ARG VERSION=v0.1.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/openwebui-proxy .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 proxy \
    && adduser -S -D -H -u 10001 -G proxy proxy
WORKDIR /app
COPY --from=build /out/openwebui-proxy /usr/local/bin/openwebui-proxy
USER 10001:10001
ENV HOST=0.0.0.0 PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/openwebui-proxy"]
