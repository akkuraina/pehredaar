# Pehredaar in one container: the Go binary plus pdftotext (poppler).
# Build:  docker build -t pehredaar .
# Run:    docker run -p 8788:8788 -v pehredaar-data:/data --env-file .env pehredaar

# --- build ---------------------------------------------------------------
FROM golang:1.27 AS build
WORKDIR /src
# Dependencies first, so editing code doesn't re-download modules.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0: the SQLite driver is pure Go, so the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pehredaar ./cmd/pehredaar

# --- run -----------------------------------------------------------------
FROM debian:trixie-slim
# poppler-utils provides pdftotext. Pehredaar only uses the flags poppler and
# xpdf share (-layout, -enc UTF-8). tzdata so logs can use IST.
RUN apt-get update \
 && apt-get install -y --no-install-recommends poppler-utils ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
RUN useradd --system --uid 10001 --home-dir /app pehredaar \
 && mkdir -p /data && chown pehredaar /data
WORKDIR /app
COPY --from=build /out/pehredaar /usr/local/bin/pehredaar
COPY sources.yaml /app/sources.yaml
USER pehredaar

# The database lives on a volume, so it survives deploys. Mount one at /data.
VOLUME /data
ENV PEHREDAAR_DB=/data/pehredaar.db \
    PEHREDAAR_SOURCES=/app/sources.yaml \
    TZ=Asia/Kolkata \
    PORT=8788
# Railway and Render set PORT themselves; the site listens on 0.0.0.0:$PORT.
EXPOSE 8788
CMD ["pehredaar", "run"]
