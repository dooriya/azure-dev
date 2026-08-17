#!/bin/bash

set -euo pipefail

EXTENSION_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXTENSION_DIR"

EXTENSION_ID_SAFE="${EXTENSION_ID//./-}"
OUTPUT_DIR="${OUTPUT_DIR:-$EXTENSION_DIR/bin}"
mkdir -p "$OUTPUT_DIR"

COMMIT=$(git rev-parse HEAD)
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

if [ -n "${EXTENSION_PLATFORM:-}" ]; then
    PLATFORMS=("$EXTENSION_PLATFORM")
else
    PLATFORMS=(
        "windows/amd64"
        "windows/arm64"
        "darwin/amd64"
        "darwin/arm64"
        "linux/amd64"
        "linux/arm64"
    )
fi

VERSION_PATH="$EXTENSION_ID/internal/version"
for PLATFORM in "${PLATFORMS[@]}"; do
    OS=$(echo "$PLATFORM" | cut -d'/' -f1)
    ARCH=$(echo "$PLATFORM" | cut -d'/' -f2)
    OUTPUT_NAME="$OUTPUT_DIR/$EXTENSION_ID_SAFE-$OS-$ARCH"
    if [ "$OS" = "windows" ]; then
        OUTPUT_NAME+='.exe'
    fi

    echo "Building for $OS/$ARCH..."
    rm -f "$OUTPUT_NAME"
    GOOS=$OS GOARCH=$ARCH go build \
        -ldflags="-X '$VERSION_PATH.Version=$EXTENSION_VERSION' -X '$VERSION_PATH.Commit=$COMMIT' -X '$VERSION_PATH.BuildDate=$BUILD_DATE'" \
        -o "$OUTPUT_NAME"
done

echo "Build completed successfully!"
echo "Binaries are located in the $OUTPUT_DIR directory."

