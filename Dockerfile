# syntax=docker/dockerfile:1
# Hookwitness container image. The build stage runs on the build machine's
# platform and cross-compiles for the target (pure Go, no cgo), so a
# multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=none
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH make build VERSION=$VERSION COMMIT=$COMMIT \
    && mkdir -p /out/data && cp bin/hookwitness /out/

# distroless static, nonroot: no shell, CA certificates and tzdata, uid 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
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
# 8080 catches; 8081 is the admin's own port, used only when
# HOOKWITNESS_ADMIN_ADDR=:8081 is set (by default the admin is under /admin on 8080).
EXPOSE 8080 8081
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/hookwitness", "health"]
ENTRYPOINT ["/hookwitness"]
CMD ["serve"]
