#!/bin/sh
# Regenerate the gRPC stubs. Requires protoc, protoc-gen-go and protoc-gen-go-grpc.
set -e
protoc -I api/proto \
    --go_out=. --go_opt=module=github.com/itmisx/snowid-server \
    --go-grpc_out=. --go-grpc_opt=module=github.com/itmisx/snowid-server \
    api/proto/snowid/v1/snowid.proto
