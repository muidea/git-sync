#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-muidea/git-sync}"
BINARY_NAME="git-backup"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-}"
TMP_DIR=""

cleanup() {
  if [[ -n "${TMP_DIR}" && -d "${TMP_DIR}" ]]; then
    rm -rf "${TMP_DIR}"
  fi
}
trap cleanup EXIT

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)
      echo "ERROR: unsupported OS $(uname -s)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *)
      echo "ERROR: unsupported architecture $(uname -m)" >&2
      exit 1
      ;;
  esac
}

resolve_latest_version() {
  local effective_url tag
  effective_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")"
  tag="${effective_url##*/}"
  if [[ -z "${tag}" || "${tag}" == "latest" ]]; then
    echo "ERROR: failed to resolve latest release tag from ${effective_url}" >&2
    exit 1
  fi
  echo "${tag#v}"
}

verify_checksum() {
  local dir="$1" archive="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "${dir}" && grep " ${archive}\$" checksums.txt | sha256sum -c -)
  elif command -v shasum >/dev/null 2>&1; then
    (cd "${dir}" && grep " ${archive}\$" checksums.txt | shasum -a 256 -c -)
  else
    echo "WARNING: sha256sum/shasum not found, skipping checksum verification" >&2
  fi
}

main() {
  local os arch ver archive base_url extracted_bin dest

  os="$(detect_os)"
  arch="$(detect_arch)"

  if [[ -z "${VERSION}" ]]; then
    ver="$(resolve_latest_version)"
  else
    ver="${VERSION#v}"
  fi

  archive="${BINARY_NAME}_${ver}_${os}_${arch}.tar.gz"
  base_url="https://github.com/${REPO}/releases/download/v${ver}"

  TMP_DIR="$(mktemp -d)"

  echo "Installing ${BINARY_NAME} v${ver} (${os}/${arch})..."
  curl -fsSL "${base_url}/${archive}" -o "${TMP_DIR}/${archive}"
  curl -fsSL "${base_url}/checksums.txt" -o "${TMP_DIR}/checksums.txt"

  verify_checksum "${TMP_DIR}" "${archive}"
  tar -xzf "${TMP_DIR}/${archive}" -C "${TMP_DIR}"

  extracted_bin="${TMP_DIR}/${BINARY_NAME}_${ver}_${os}_${arch}/${BINARY_NAME}"
  dest="${INSTALL_DIR}/${BINARY_NAME}"

  if [[ -w "${INSTALL_DIR}" ]] || [[ "$(id -u)" -eq 0 ]]; then
    mkdir -p "${INSTALL_DIR}"
    install -m 0755 "${extracted_bin}" "${dest}"
  else
    sudo mkdir -p "${INSTALL_DIR}"
    sudo install -m 0755 "${extracted_bin}" "${dest}"
  fi

  echo "Installed: $("${dest}" version)"
}

main "$@"
