# The web UI is built and embedded in the server: one static binary, no CDN.
FROM node:26-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npx tsc -b && npx vite build --outDir /out/web

FROM golang:1.27-alpine AS server
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
COPY --from=web /out/web ./web
RUN go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/observatory .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=server /out/observatory /observatory
EXPOSE 8080 4318
ENTRYPOINT ["/observatory"]
