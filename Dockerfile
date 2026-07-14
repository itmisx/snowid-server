# Build
#
# --platform=$BUILDPLATFORM pins the build stage to the machine doing the
# building, and Go then cross-compiles to $TARGETARCH. The alternative — letting
# buildx run an arm64 toolchain under QEMU — compiles the same code an order of
# magnitude slower, for no benefit: nothing here uses cgo, so cross-compiling is
# just an environment variable.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src

# Cache dependencies separately from the source, so editing code does not
# re-download the module graph. (.dockerignore keeps docs and .git out of the
# context, which is what stops a README edit from invalidating this layer.)
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Supplied by buildx, one value per platform being built.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/snowid-server ./cmd/snowid-server

# Run
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/snowid-server /snowid-server
EXPOSE 50051
# 65532 numerically, not the name "nonroot". A username here would break the
# manifest's runAsNonRoot: kubelet cannot check whether a *name* is root, so it
# refuses to start the container at all (CreateContainerConfigError).
USER 65532:65532
ENTRYPOINT ["/snowid-server"]
