# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads independently of source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pure-Go sqlite (modernc.org/sqlite) means CGO=0 - static binary,
# trivial to drop into distroless.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') \
    go build -ldflags="-s -w" -o /out/hostthisd ./cmd/hostthisd

# ---- runtime stage ----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

# Distroless ships /etc/passwd with `nonroot` (uid 65532). Our data
# dir must be writable by that user; compose mounts a volume that
# we chown via an init/setup step if needed.
WORKDIR /app

COPY --from=build /out/hostthisd /app/hostthisd
COPY web/landing.html /app/web/landing.html

EXPOSE 2222 8080

# Default config - operator overrides via flags or HOSTTHIS_* envs.
ENV HOSTTHIS_DATA_DIR=/var/lib/hostthis \
    HOSTTHIS_SSH_ADDR=:2222 \
    HOSTTHIS_HTTP_ADDR=:8080 \
    HOSTTHIS_LANDING=/app/web/landing.html

USER nonroot:nonroot

ENTRYPOINT ["/app/hostthisd"]
