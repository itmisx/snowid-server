.PHONY: test lint build proto docker docker-multiarch run

# Stamped into the binary, and reported in the "generator ready" log line. Without
# it every image built here would call itself "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# What the release workflow publishes to. Overridable, so a fork can push its own.
IMAGE ?= ghcr.io/itmisx/snowid-server

# The architectures released. Nothing here uses cgo, so both cross-compile from
# whatever machine you are on — see the Dockerfile.
PLATFORMS ?= linux/amd64,linux/arm64

test:
	go test -race -count=1 ./...

lint:
	@# gofmt -l only PRINTS the offending files; it exits 0 either way, so on its
	@# own this target can never fail. Turn the output into the exit status.
	@out="$$(gofmt -l ./cmd ./internal ./pkg)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...

build:
	go build -ldflags "-X main.version=$(VERSION)" -o snowid-server ./cmd/snowid-server

proto:
	./buf.gen.sh

# One image, this machine's architecture, loaded into the local docker daemon so
# you can run it.
docker:
	docker build --build-arg VERSION=$(VERSION) -t snowid-server:$(VERSION) .

# Both release architectures, the same way the release workflow does it. Nothing
# is pushed: buildx cannot load a multi-platform image into the docker daemon, so
# this only proves it BUILDS. Use it to check a Dockerfile change before tagging.
docker-multiarch:
	docker buildx build --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

run: build
	./snowid-server --worker-id 0
