# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /build/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.7-alpine AS build
RUN apk add --no-cache bash
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/embed.go ./web/embed.go
COPY --from=web /build/web/dist ./web/dist
COPY scripts/build.sh ./scripts/build.sh
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=2026.9.2
ARG BUILD_DATE=2026-09-21
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH VERSION=$VERSION BUILD_DATE=$BUILD_DATE OUTPUT=/out/janus bash scripts/build.sh
COPY LICENSE COMMERCIAL-TERMS.md NOTICE THIRD_PARTY_NOTICES.md /out/licenses/
RUN mkdir -p /out/data && chmod 0700 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=2026.9.2
LABEL org.opencontainers.image.title="Janus Edge"       org.opencontainers.image.vendor="Torvanis"       org.opencontainers.image.licenses="Elastic-2.0"       org.opencontainers.image.source="https://github.com/Torvanis/janus"       org.opencontainers.image.version=$VERSION
COPY --from=build /out/janus /janus
COPY --from=build /out/licenses/ /usr/share/licenses/janus/
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV JANUS_LISTEN_ADDR=:8080 JANUS_DEV_AUTH=false
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=6s --start-period=30s CMD ["/janus", "--health-probe"]
ENTRYPOINT ["/janus"]
