#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: build.sh [--override-version <version>] [arch]" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION=""
ARCH=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --override-version)
      [[ $# -ge 2 ]] || usage
      VERSION="$2"
      shift 2
      ;;
    --override-version=*)
      VERSION="${1#*=}"
      shift
      ;;
    -h|--help)
      usage
      ;;
    -*)
      echo "Unknown flag: $1" >&2
      usage
      ;;
    *)
      if [[ -z "${ARCH}" ]]; then
        ARCH="$1"
        shift
      else
        echo "Unexpected argument: $1" >&2
        usage
      fi
      ;;
  esac
done

if [[ -z "${VERSION}" ]]; then
  VERSION_FILE="${SCRIPT_DIR}/VERSION"
  [[ -f "${VERSION_FILE}" ]] || { echo "VERSION file not found at ${VERSION_FILE}" >&2; exit 1; }
  VERSION="$(tr -d '[:space:]' < "${VERSION_FILE}")"
  [[ -n "${VERSION}" ]] || { echo "VERSION file is empty" >&2; exit 1; }
fi

ARCH="${ARCH:-arm64}"
BIN="phyhub-liot-device-provisioning"
OUTPUT="bin/${BIN}-${VERSION}-linux-${ARCH}"

mkdir -p bin

GOOS=linux GOARCH="${ARCH}" go build \
  -ldflags="-s -w -X main.version=${VERSION}" \
  -o "${OUTPUT}" \
  "./cmd/${BIN}"

echo "Built: ${OUTPUT}"
