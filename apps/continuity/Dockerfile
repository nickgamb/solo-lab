# continuity-controller: static Go binary on distroless (nonroot).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /continuity .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /continuity /continuity
USER 65532:65532
ENTRYPOINT ["/continuity"]
