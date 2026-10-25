# Build: make image   (or: podman build -f Containerfile -t ccdav .)
# The build stage runs on the build machine's platform and cross-compiles,
# so multi-arch builds (amd64/arm64) need no emulation for Go.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ccdav ./cmd/ccdav

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ccdav /usr/local/bin/ccdav
ENV CCDAV_DATA=/data CCDAV_LISTEN=:8080 CCDAV_LOG=json
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ccdav"]
