# syntax=docker/dockerfile:1
# Router image: Go server + built dashboard on a distroless, non-root base.

FROM --platform=$BUILDPLATFORM node:24-alpine AS web
RUN npm install -g pnpm@12.21.0
WORKDIR /web
COPY dashboard/package.json dashboard/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY dashboard/ ./
RUN pnpm build

# Cross-compile on the build platform (pure Go, no cgo) instead of emulating.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/umm-server ./cmd/umm-server \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/umm ./cmd/umm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/umm-server /out/umm /usr/local/bin/
COPY --from=web /web/dist /app/web
ENV UMM_LISTEN=:8080
WORKDIR /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/umm-server"]
CMD ["serve", "-config", "/etc/umm/server.yaml"]
