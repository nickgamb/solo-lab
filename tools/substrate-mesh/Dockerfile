# Substrate components as `ko build` makes them: a static Go binary at
# /ko-app/<name> on distroless static (root), same env and entrypoint.
FROM golang:1.26.3 AS build
ARG VERSION
WORKDIR /src
COPY . .
RUN for c in ateapi atecontroller ateom-gvisor; do \
      CGO_ENABLED=0 go build -mod=vendor -trimpath \
        -ldflags "-X=github.com/agent-substrate/substrate/internal/version.Version=${VERSION}" \
        -o /out/$c ./cmd/$c || exit 1; done

FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0 AS base
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/ko-app \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt KO_DATA_PATH=/var/run/ko

FROM base AS ateapi
COPY --from=build /out/ateapi /ko-app/ateapi
ENTRYPOINT ["/ko-app/ateapi"]

FROM base AS atecontroller
COPY --from=build /out/atecontroller /ko-app/atecontroller
ENTRYPOINT ["/ko-app/atecontroller"]

FROM base AS ateom-gvisor
COPY --from=build /out/ateom-gvisor /ko-app/ateom-gvisor
ENTRYPOINT ["/ko-app/ateom-gvisor"]
