# Build: make image   (or: podman build -f Containerfile -t davd .)
FROM docker.io/library/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/davd ./cmd/davd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/davd /usr/local/bin/davd
ENV DAVD_DATA=/data DAVD_LISTEN=:8080 DAVD_LOG=json
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/davd", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/davd"]
