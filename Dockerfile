# syntax=docker/dockerfile:1.8@sha256:e87caa74dcb7d46cd820352bfea12591f3dba3ddc4285e19c7dcd13359f7cefd
ARG BUF_VERSION=1.66.0
# Release sha256.txt entries for buf-Linux-x86_64 and buf-Linux-aarch64.
ARG BUF_SHA256_X86_64=32e8e7b236e1b9da4eb20ad3c7701a404f7909b18d60f6d00837c63b31d4e6cf
ARG BUF_SHA256_AARCH64=f8a77dbcbf492836e0e1bca2f2c9dbb5392a5bb47db161be89d811f36b86ee30

# Images are pinned to the index digests of their tags (golang:1.25-alpine
# was Go 1.25.14 on 2026-10-02). Update each tag and digest together.

FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS buf
ARG BUF_VERSION BUF_SHA256_X86_64 BUF_SHA256_AARCH64
RUN apk add --no-cache curl
RUN set -eu; \
    arch="$(uname -m)"; \
    case "${arch}" in \
      x86_64) sha256="${BUF_SHA256_X86_64}" ;; \
      aarch64) sha256="${BUF_SHA256_AARCH64}" ;; \
      *) echo "no pinned buf checksum for ${arch}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL \
      "https://github.com/bufbuild/buf/releases/download/v${BUF_VERSION}/buf-Linux-${arch}" \
      -o /usr/local/bin/buf; \
    echo "${sha256}  /usr/local/bin/buf" | sha256sum -c -; \
    chmod +x /usr/local/bin/buf

FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build

# buf reads the pinned API revision from a git repository.
RUN apk add --no-cache git

WORKDIR /src

COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY buf.gen.yaml buf.yaml ./
RUN buf generate --include-imports

COPY . .

ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/agents ./cmd/agents-service

FROM alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507 AS runtime

WORKDIR /app

COPY --from=build /out/agents /app/agents

RUN addgroup -g 10001 -S app && adduser -u 10001 -S app -G app

USER 10001

ENTRYPOINT ["/app/agents"]
