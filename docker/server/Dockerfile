# ---- Build stage ----
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /conquest ./cmd/conquest/

# ---- Migrate binary ----
# Built from source with the same Go toolchain rather than downloaded: the
# release binaries are built with an old Go and link every database driver
# (with their dependencies' findings). Only the postgres driver is needed
# (the chart's DATABASE_URL is postgres://); the file source is always
# built in. go install verifies the module against the Go checksum
# database. Keep the version in step with .github/workflows/ci.yaml.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS migrate-builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG MIGRATE_VERSION=v4.20.1

# A cross-compiled go install lands in $GOPATH/bin/<os>_<arch>/, a native
# one in $GOPATH/bin/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go install -tags postgres \
    -ldflags="-s -w -X main.Version=${MIGRATE_VERSION}" \
    "github.com/golang-migrate/migrate/v4/cmd/migrate@${MIGRATE_VERSION}" \
    && find "$(go env GOPATH)/bin" -type f -name migrate -exec mv {} /migrate \; \
    && test -x /migrate

# ---- Runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 conquest

COPY --from=builder /conquest /usr/local/bin/conquest
COPY --from=builder /src/migrations /migrations
COPY --from=migrate-builder /migrate /usr/local/bin/migrate

USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/conquest"]
