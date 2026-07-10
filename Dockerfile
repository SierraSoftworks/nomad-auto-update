# syntax=docker/dockerfile:1

# Cross-compile for the target platform on the build host rather than under
# emulation — Go cross-compiles natively, which keeps multi-platform image
# builds fast.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/nomad-auto-update .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nomad-auto-update /usr/local/bin/nomad-auto-update

ENTRYPOINT ["/usr/local/bin/nomad-auto-update"]
