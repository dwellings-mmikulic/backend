# syntax=docker/dockerfile:1

# --- Build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build the static binary.
COPY . .
# The maintenance commands ship in the same image: they need the same env and
# the same ffmpeg, and are run with `compose run --entrypoint`.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
        -o /out/ ./cmd/server ./cmd/backfill-hls ./cmd/backfill-videos

# --- Runtime stage ---
# Alpine (not distroless) because we need the ffmpeg binary + a TTF font for the
# drawtext overlays.
FROM alpine:3.20
RUN apk add --no-cache ffmpeg font-dejavu ca-certificates && \
    adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/backfill-hls /app/backfill-hls
COPY --from=build /out/backfill-videos /app/backfill-videos
# Bundled CC0 music tracks used for the video soundtrack.
COPY assets/ /app/assets/
ENV VIDEO_FONT_PATH=/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf \
    MUSIC_DIR=/app/assets/music
USER app
ENTRYPOINT ["/app/server"]
