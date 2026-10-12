# syntax=docker/dockerfile:1
# Hookwitness container image. The build stage runs on the build machine's
# platform and cross-compiles for the target (pure Go, no cgo), so a
# multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=none
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH make build VERSION=$VERSION COMMIT=$COMMIT \
    && mkdir -p /out/data && cp bin/hookwitness /out/

# distroless static, nonroot: no shell, CA certificates and tzdata, uid 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG VERSION=dev COMMIT=none
LABEL org.opencontainers.image.title="hookwitness" \
      org.opencontainers.image.description="Self-hosted webhook catcher with verdicts" \
      org.opencontainers.image.source="https://github.com/petroladkin/hookwitness" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
COPY --from=build /out/hookwitness /hookwitness
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/doc/hookwitness/
# A named volume mounted here starts as a copy of this directory, so the
# image user owns it and can create the database and its key file.
COPY --from=build --chown=65532:65532 /out/data /data
ENV HOOKWITNESS_ADDR=:8080 \
    HOOKWITNESS_DB=/data/hookwitness.db
VOLUME /data
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/hookwitness", "health"]
ENTRYPOINT ["/hookwitness"]
CMD ["serve"]
