#!/usr/bin/env bash
#
# Regenerate internal/control/proto/*.pb.go from control.proto.
#
# Generated files are committed so release builds do not depend on protoc.
# Tool versions are pinned; run this script and commit the result whenever the
# .proto changes. CI can re-run it and check that git diff is empty.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "${SCRIPT_DIR}"

PROTOC_GEN_GO_VERSION="v1.36.6"
PROTOC_GEN_GO_GRPC_VERSION="v1.5.1"
TOOLS_DIR="${SCRIPT_DIR}/.tools"

if ! command -v protoc >/dev/null 2>&1; then
    echo "error: protoc not found (install protobuf-compiler)" >&2
    exit 1
fi

mkdir -p "${TOOLS_DIR}"
if [[ ! -x "${TOOLS_DIR}/protoc-gen-go" ]]; then
    GOBIN="${TOOLS_DIR}" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
fi
if [[ ! -x "${TOOLS_DIR}/protoc-gen-go-grpc" ]]; then
    GOBIN="${TOOLS_DIR}" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
fi

# Locate the well-known type includes next to the protoc binary (homebrew and
# /usr/local layouts both work).
WKT_INCLUDE="$(dirname "$(command -v protoc)")/../include"

PATH="${TOOLS_DIR}:${PATH}" protoc \
    -I "${SCRIPT_DIR}/internal/control/proto" \
    -I "${WKT_INCLUDE}" \
    --go_out="${SCRIPT_DIR}/internal/control/proto" \
    --go_opt=paths=source_relative \
    --go-grpc_out="${SCRIPT_DIR}/internal/control/proto" \
    --go-grpc_opt=paths=source_relative \
    "${SCRIPT_DIR}/internal/control/proto/control.proto"

echo "generated: internal/control/proto/control.pb.go internal/control/proto/control_grpc.pb.go"
