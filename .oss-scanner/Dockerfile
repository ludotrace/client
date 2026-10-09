# Build image for Anthropic's OSS Scanner (github.com/anthropics/oss-scanner).
# Built with this repository as the context; network access exists only during the build, so every
# module and tool the tests need is fetched here.
FROM debian:bookworm
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git build-essential pkg-config \
      libgtk-3-dev libayatana-appindicator3-dev \
    && rm -rf /var/lib/apt/lists/*

# scanner contract: the checkout lives inside the image, at /src
COPY . /src
WORKDIR /src

# Toolchain version comes from the go directive in go.mod, the same source CI pins from.
RUN GOVER="$(awk '/^go /{print $2}' go.mod)" \
    && curl -fsSL "https://go.dev/dl/go${GOVER}.linux-amd64.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH \
    GOFLAGS=-mod=mod \
    GOTOOLCHAIN=local

RUN go mod download

# Both Linux builds, as `make build-linux` and `make build-linux-headless` produce them, without
# optimisations so debuggers see every variable.
RUN mkdir -p dist \
    && CGO_ENABLED=1 go build -gcflags=all="-N -l" -o dist/ludotrace-linux ./cmd/ludotrace \
    && CGO_ENABLED=0 go build -tags headless -gcflags=all="-N -l" -o dist/ludotrace-linux-headless ./cmd/ludotrace

# Compile and run the tests once so the build cache holds the race-enabled test binaries. Tests that
# need a desktop session or a keyring can fail here without failing the image.
RUN go test -race ./... || echo "WARNING: go test reported failures"
