# idtoken-exchange: static Go binary on distroless (nonroot). Standard library only.
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /idtoken-exchange .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /idtoken-exchange /idtoken-exchange
USER 65532:65532
ENTRYPOINT ["/idtoken-exchange"]
