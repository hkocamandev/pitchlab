# One image for every Go binary in the project.
#
# They share a module, a domain model and a repository layer, so building them
# separately would mean four almost identical Dockerfiles and four copies of
# the dependency download. Which binary runs is chosen at start time by the
# compose command, not at build time.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first and on their own layer: the module files change far less
# often than the source, so editing a handler does not re-download the world.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO is off, so the result is a static binary that runs on a distroless-style
# base with no libc to match. -trimpath keeps build machine paths out of the
# binary, and the ldflags drop the symbol table and DWARF data: nothing here
# is debugged by attaching to a production container, and the image is
# meaningfully smaller without them.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/processor ./cmd/processor && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w" -o /out/replay ./cmd/replay && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w" -o /out/seed ./cmd/seed && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w" -o /out/dlq-replay ./cmd/dlq-replay && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w" -o /out/wsclient ./cmd/wsclient

FROM alpine:3.21

# wget for the health check, and certificates in case anything ever talks to a
# TLS endpoint. Nothing else: every package in a runtime image is a package
# somebody has to keep patched.
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -u 10001 pitchlab

COPY --from=build /out/ /usr/local/bin/

# Not root. Nothing these binaries do needs it, and a container that has it is
# a container that can be asked to use it.
USER pitchlab

# Overridden per service in compose. Defaulting to the API means a bare
# `docker run` does something sensible rather than exiting.
ENTRYPOINT ["/usr/local/bin/api"]
