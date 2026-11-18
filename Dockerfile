# syntax=docker/dockerfile:1
#
# Dockerfile
# The feasible binary in a minimal image, for people who run everything in containers.
#
# Created: 2026-11-18
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#
# The binary is the product and the image only carries it. Configure it with the
# same FEASIBLE_* variables as the binary; the image sets the few that have to
# differ inside a container (listen addresses and the data directory).
#
#   docker build -t feasible .
#

# Compiles one binary per target platform. It runs on the build machine and
# cross-compiles, so a multi-arch build needs no emulation. The release workflow
# replaces this stage with the archive binaries it has already built and tested,
# by passing a build context with the same name, so a published image holds the
# same bytes as the release download.
FROM --platform=$BUILDPLATFORM golang:1.26 AS binaries

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
	-ldflags "-s -w \
	-X github.com/Feasible-Analytics/app.feasible.lol/internal/build.Version=${VERSION} \
	-X github.com/Feasible-Analytics/app.feasible.lol/internal/build.Commit=${COMMIT} \
	-X github.com/Feasible-Analytics/app.feasible.lol/internal/build.Date=${DATE}" \
	-o /${TARGETOS}/${TARGETARCH}/feasible ./cmd/feasible

# Distroless static has the two things the binary reads from the system: time
# zone data, without which no site can be added, and CA certificates, without
# which nothing outbound works over HTTPS. It has no shell and no package
# manager, and its nonroot user is uid 65532.
FROM gcr.io/distroless/static-debian13:nonroot

ARG TARGETOS
ARG TARGETARCH

LABEL org.opencontainers.image.title="Feasible" \
	org.opencontainers.image.description="Privacy-first web analytics. One Go binary, one SQLite database per account." \
	org.opencontainers.image.url="https://feasible.lol" \
	org.opencontainers.image.documentation="https://feasible.lol/docs/self-hosting/" \
	org.opencontainers.image.source="https://github.com/Feasible-Analytics/app.feasible.lol" \
	org.opencontainers.image.licenses="AGPL-3.0-or-later" \
	org.opencontainers.image.vendor="Cloudmanic Labs, LLC"

COPY --from=binaries /${TARGETOS}/${TARGETARCH}/feasible /usr/local/bin/feasible

# Production and self-hosted mode, so a missing salt or support address stops
# the process with the variable's name, and public sign-up stays off. Listening
# on every interface is what makes the published port reach the process.
ENV FEASIBLE_ENV=production \
	FEASIBLE_APP_HOSTED=false \
	FEASIBLE_APP_LISTEN=:19301 \
	FEASIBLE_INGEST_LISTEN=:19302 \
	FEASIBLE_APP_DATA_DIR=/data \
	FEASIBLE_INGEST_BUFFER_PATH=/data/ingest/buffer.db

# Created after USER so it belongs to nonroot, which a new named volume then
# inherits. It is also the working directory, so the log mail transport's
# tmp/mail/ lands in the volume rather than in a read-only layer.
USER nonroot:nonroot
WORKDIR /data
VOLUME /data

# 19301 is serve, which is all a self-hosted install runs. 19302 is the separate
# ingest process, for anyone who splits the two.
EXPOSE 19301 19302

ENTRYPOINT ["/usr/local/bin/feasible"]
CMD ["serve"]
