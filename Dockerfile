# Server image for the NAS. Cross-compiles natively on the build host (no QEMU)
# for each target platform, then ships a static binary on distroless.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X photoframe/internal/version.Version=${VERSION}" \
    -o /out/photoframe-server ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/photoframe-server /photoframe-server
ENV DATA_DIR=/data LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/photoframe-server", "-healthcheck"]
ENTRYPOINT ["/photoframe-server"]
