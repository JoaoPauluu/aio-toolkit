# syntax=docker/dockerfile:1

# ---- build the Go server (single static binary) ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
# Front-end libraries are vendored at build time so the UI works offline.
ADD https://cdn.jsdelivr.net/npm/pdfjs-dist@4.10.38/build/pdf.min.mjs        web/static/vendor/pdf.min.mjs
ADD https://cdn.jsdelivr.net/npm/pdfjs-dist@4.10.38/build/pdf.worker.min.mjs web/static/vendor/pdf.worker.min.mjs
ADD https://cdn.jsdelivr.net/npm/sortablejs@1.15.6/Sortable.min.js           web/static/vendor/Sortable.min.js
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/aio ./cmd/aio

# ---- runtime: tools the plugins call ----
FROM python:3.13-alpine
RUN apk add --no-cache ffmpeg qpdf ghostscript nodejs tini \
 && python -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir --disable-pip-version-check "yt-dlp[default]" \
 && find /opt/venv -name '__pycache__' -prune -exec rm -rf {} +

# /data/venv (created by the "Update yt-dlp" plugin) takes precedence over the baked-in copy.
ENV PATH=/data/venv/bin:/opt/venv/bin:$PATH \
    AIO_ADDR=:8080 \
    AIO_DATA_DIR=/data \
    AIO_DOWNLOADS_DIR=/downloads \
    AIO_PLUGIN_DIRS=/app/plugins:/plugins \
    AIO_SDK_DIR=/app/sdk/python \
    HOME=/data/home \
    XDG_CACHE_HOME=/data/cache \
    PYTHONDONTWRITEBYTECODE=1 \
    GOMEMLIMIT=48MiB

COPY --from=build /out/aio /usr/local/bin/aio
COPY sdk /app/sdk
COPY plugins /app/plugins
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh && mkdir -p /data /downloads /plugins && chmod 0777 /data /downloads /plugins

EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1
ENTRYPOINT ["/sbin/tini", "--", "/entrypoint.sh"]
CMD ["aio"]
