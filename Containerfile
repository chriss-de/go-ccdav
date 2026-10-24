# Build: make image   (or: podman build -f Containerfile -t ccdav .)
FROM docker.io/library/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ccdav ./cmd/ccdav

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ccdav /usr/local/bin/ccdav
ENV CCDAV_DATA=/data CCDAV_LISTEN=:8080 CCDAV_LOG=json
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/ccdav", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/ccdav"]
