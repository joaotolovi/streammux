# Build stage — builds the frontend (web/) and then the Go binary (which embeds it).
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache nodejs npm git
WORKDIR /build

# Frontend deps (cached separately from source changes).
COPY web/package.json web/package-lock.json ./
COPY web/package.json web/package-lock.json web/
RUN cd web && npm ci

# Frontend source + build.
COPY web/ web/
RUN cd web && npm run build

# Backend deps (cached separately from source changes).
COPY go.mod go.sum ./
RUN go mod download

# Backend source + build.
COPY . .
RUN CGO_ENABLED=0 go build -o /streammux ./cmd/streammux

# Runtime stage — Alpine Edge with static FFmpeg 9 binaries.
FROM mwader/static-ffmpeg:9.0 AS ffmpeg

FROM alpine:edge
RUN apk add --no-cache ca-certificates fontconfig font-dejavu font-noto-emoji
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg /ffprobe /usr/local/bin/ffprobe
COPY --from=builder /streammux /usr/local/bin/streammux

ENV PORT=3001
EXPOSE 3001
VOLUME ["/data"]

CMD ["streammux"]
