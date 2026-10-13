# The kopia adapter: kopia's own image plus a maison-engine binary.
#
# Building FROM a pinned kopia is what makes the engine version an attribute of THIS
# image rather than a number Maison and the PCS template have to agree about in two
# places. Never a floating tag: an engine that changes under a live repository turns a
# format surprise into a 3am failure.
#
# Bumping kopia is a change to this line and nothing else.
ARG KOPIA_VERSION=0.23.1

# --platform=$BUILDPLATFORM pins the build stage to the RUNNER's architecture, and the
# Go toolchain cross-compiles to the target from there. Without it buildx runs this whole
# stage under QEMU for every non-native platform — a Go build emulated instruction by
# instruction, for a static binary that cross-compiles for free.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
# No third-party dependencies, so go.mod is the whole of the module graph and there is
# no download step to cache.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
# CGO off so the binary runs in whatever base the engine image happens to use, whether or
# not it carries a libc the build stage would have linked against.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/maison-engine ./cmd/maison-engine

FROM kopia/kopia:${KOPIA_VERSION}
COPY --from=build /out/maison-engine /usr/local/bin/maison-engine

# The image's own reference, for the adapter.json `serve` writes. Nothing inside a
# container can learn which image it was started from without the Docker socket, so the
# build states it — CI passes the published tag. A compose file that pins the image by a
# different spelling sets MAISON_ENGINE_IMAGE itself, and that wins: Maison compares the
# descriptor against the container exactly as it was declared.
ARG IMAGE_REF=""
ENV MAISON_ENGINE_IMAGE=${IMAGE_REF}

# The adapter is what this image is FOR, so it is the entrypoint. The UI container
# overrides it to run `kopia server`; the resident adapter either keeps it and runs
# `serve`, or (on a deployment whose host provisions the repository) runs a sleep, and is
# exec'd into either way — Maison names the binary explicitly in both modes.
# Declaring it here is what makes a bare `docker run <image> capabilities` work, and what
# stops the image's identity depending on which flag the caller remembered.
ENTRYPOINT ["/usr/local/bin/maison-engine"]
