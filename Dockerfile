# ---- build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
# No external dependencies, so just copy and build (templates are go:embed'd).
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /openiq .

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=build /openiq /usr/local/bin/openiq

# Token cache lives on a volume so the session survives restarts.
ENV ADDR=:8087 \
    GARMIN_TOKEN_FILE=/data/tokens.json
RUN mkdir -p /data && chown app /data
VOLUME /data
USER app
EXPOSE 8087

ENTRYPOINT ["/usr/local/bin/openiq"]
