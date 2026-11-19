#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-muidea/git-sync}"
BINARY_NAME="git-backup"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-}"
GITHUB_BASE="${GITHUB_BASE:-https://github.com}"
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

check_host_deps() {
  if ! command -v git >/dev/null 2>&1; then
    echo "WARNING: 'git' is not installed; git >= 2.31 is required at runtime." >&2
    return
  fi
  local ver_str major minor
  ver_str="$(git --version | awk '{print $3}')"
  major="${ver_str%%.*}"
  minor="${ver_str#*.}"
  minor="${minor%%.*}"
  if [[ -n "${major}" && -n "${minor}" ]]; then
    if (( major < 2 || (major == 2 && minor < 31) )); then
      echo "WARNING: found git ${ver_str}, but git >= 2.31 is required (or use the Docker deployment)." >&2
    fi
  fi
  if ! command -v git-lfs >/dev/null 2>&1; then
    echo "NOTE: 'git-lfs' is not installed (only required if backing up repositories with Git LFS)." >&2
  fi
}

resolve_latest_version() {
  local effective_url tag
  effective_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${GITHUB_BASE%/}/${REPO}/releases/latest")"
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
  local os arch ver archive base_url extracted_bin dest is_upgrade=0

  os="$(detect_os)"
  arch="$(detect_arch)"
  check_host_deps

  if [[ -z "${VERSION}" ]]; then
    ver="$(resolve_latest_version)"
  else
    ver="${VERSION#v}"
  fi

  archive="${BINARY_NAME}_${ver}_${os}_${arch}.tar.gz"
  base_url="${GITHUB_BASE%/}/${REPO}/releases/download/v${ver}"

  TMP_DIR="$(mktemp -d)"

  dest="${INSTALL_DIR}/${BINARY_NAME}"
  if [[ -x "${dest}" ]]; then
    is_upgrade=1
  fi

  echo "Installing ${BINARY_NAME} v${ver} (${os}/${arch})..."
  curl -fsSL "${base_url}/${archive}" -o "${TMP_DIR}/${archive}"
  curl -fsSL "${base_url}/checksums.txt" -o "${TMP_DIR}/checksums.txt"

  verify_checksum "${TMP_DIR}" "${archive}"
  tar -xzf "${TMP_DIR}/${archive}" -C "${TMP_DIR}"

  extracted_bin="${TMP_DIR}/${BINARY_NAME}_${ver}_${os}_${arch}/${BINARY_NAME}"

  if [[ -w "${INSTALL_DIR}" ]] || [[ "$(id -u)" -eq 0 ]]; then
    mkdir -p "${INSTALL_DIR}"
    install -m 0755 "${extracted_bin}" "${dest}"
  else
    sudo mkdir -p "${INSTALL_DIR}"
    sudo install -m 0755 "${extracted_bin}" "${dest}"
  fi

  echo "Installed: $("${dest}" version)"

  if [[ "${is_upgrade}" -eq 0 ]]; then
    cat <<'EOF'

Next steps:
  1. Initialize config & verify token/repos:
       git-backup init
  2. Install & enable periodic systemd timer:
       sudo git-backup service install        # or: git-backup service install --user
EOF
  fi
}

main "$@"
