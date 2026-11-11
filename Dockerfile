# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
FROM golang:1.27.3-alpine AS build

# The image only bootstraps the build: go.mod's toolchain line picks the
# compiler, fetched and checksum-verified by the go command, so a Go patch
# release does not wait for a base image.
ENV GOTOOLCHAIN=auto

WORKDIR /src

# Cache module downloads independently of source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# No cgo dependencies, so CGO_ENABLED=0 gives a static binary that drops
# straight into distroless.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') \
    go build -ldflags="-s -w" -o /out/hostthisd ./cmd/hostthisd

# ---- runtime stage ----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

# Distroless ships /etc/passwd with `nonroot` (uid 65532). The data
# dir must be writable by that user; `make data-dir-perms` chowns the
# host dir compose mounts.
WORKDIR /app

COPY --from=build /out/hostthisd /app/hostthisd
COPY web/landing.html /app/web/landing.html
COPY web/llms.txt /app/web/llms.txt

EXPOSE 2222 8080

# Default config - operator overrides via flags or HOSTTHIS_* envs.
ENV HOSTTHIS_DATA_DIR=/var/lib/hostthis \
    HOSTTHIS_SSH_ADDR=:2222 \
    HOSTTHIS_HTTP_ADDR=:8080 \
    HOSTTHIS_LANDING=/app/web/landing.html

USER nonroot:nonroot

ENTRYPOINT ["/app/hostthisd"]
