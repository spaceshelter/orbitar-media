# Build context: the repository root.
#   docker build -f mediaserver/Dockerfile .
#   docker build -f mediaserver/Dockerfile --target test .   # unit + integration tests

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY mediaserver/go.mod mediaserver/go.sum ./
RUN go mod download
COPY mediaserver/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mediaserver ./cmd/mediaserver

# the tests drive the real vips/ffmpeg/file binaries
FROM build AS test
RUN apk add --no-cache vips-tools ffmpeg file
RUN go vet ./... && go test ./...

FROM alpine:3.24
RUN apk add --no-cache vips-tools ffmpeg file ca-certificates tzdata \
    # the ids of the nginx user of the old PictShare image, which owns the files on the data volume
    && addgroup -g 101 -S nginx && adduser -u 100 -S -D -H -G nginx nginx
COPY --from=build /out/mediaserver /usr/local/bin/mediaserver
COPY mediaserver/static /srv/static
ENV DATA_DIR=/var/www/data STATIC_DIR=/srv/static TMP_DIR=/tmp/mediaserver LISTEN=:80
USER nginx
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=5s CMD wget -q -O /dev/null http://127.0.0.1/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/mediaserver"]
