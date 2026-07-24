package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const installScriptFile = "install.sh"

const installScript = `#!/bin/sh
set -eu

repository="Agent-Clubhouse/Goobers"

usage() {
	printf 'Usage: install.sh vMAJOR.MINOR.PATCH [instance-path]\n' >&2
}

fail() {
	printf 'install: %s\n' "$*" >&2
	exit 1
}

if [ "${1:-}" = "--help" ] || [ "${1:-}" = "-h" ]; then
	usage
	exit 0
fi
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	usage
	exit 2
fi

version=$1
instance_path=${2:-./goobers-instance}
stable_version=${version#v}
major=${stable_version%%.*}
remaining=${stable_version#*.}
minor=${remaining%%.*}
patch=${remaining#*.}
if [ "$stable_version" = "$version" ] ||
	[ "$remaining" = "$stable_version" ] ||
	[ "$patch" = "$remaining" ]; then
	fail "release must be an exact stable tag such as v1.2.3"
fi
case "$patch" in
	*.*) fail "release must be an exact stable tag such as v1.2.3" ;;
esac
for component in "$major" "$minor" "$patch"; do
	case "$component" in
		'' | *[!0-9]* | 0[0-9]*)
			fail "release must be an exact stable tag such as v1.2.3"
			;;
	esac
done

case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "unsupported architecture: $(uname -m)" ;;
esac

archive="goobers_${version}_${os}_${arch}.tar.gz"
release_url="https://github.com/${repository}/releases/download/${version}"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/goobers-install.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0
trap 'exit 1' 1 2 15

printf 'Downloading Goobers %s for %s/%s...\n' "$version" "$os" "$arch"
curl -fsSL "${release_url}/${archive}" -o "${tmp_dir}/${archive}"
curl -fsSL "${release_url}/SHA256SUMS" -o "${tmp_dir}/SHA256SUMS"

expected=$(awk -v name="$archive" '$2 == name { print $1 }' "${tmp_dir}/SHA256SUMS")
[ -n "$expected" ] || fail "SHA256SUMS does not contain ${archive}"
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "${tmp_dir}/${archive}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "${tmp_dir}/${archive}" | awk '{ print $1 }')
else
	fail "sha256sum or shasum is required to verify the release"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for ${archive}"

tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir"
[ -f "${tmp_dir}/goobers" ] || fail "${archive} does not contain goobers"

if [ -n "${GOOBERS_INSTALL_DIR:-}" ]; then
	install_dir=$GOOBERS_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
	install_dir="${HOME}/.local/bin"
else
	fail "HOME is unset; set GOOBERS_INSTALL_DIR to choose an install directory"
fi
mkdir -p "$install_dir"
install -m 0755 "${tmp_dir}/goobers" "${install_dir}/goobers"
binary="${install_dir}/goobers"

installed_version=$("$binary" --version | awk '{ print $2 }')
[ "$installed_version" = "$version" ] ||
	fail "installed binary did not report release ${version}"

printf 'Installed %s to %s\n' "$version" "$binary"
case ":${PATH:-}:" in
	*":${install_dir}:"*) ;;
	*) printf 'Add %s to PATH before opening a new shell.\n' "$install_dir" ;;
esac
printf 'Starting guided setup for %s...\n\n' "$instance_path"
"$binary" init --guided "$instance_path"
`

func writeInstallScript(outDir string) (string, error) {
	path := filepath.Join(outDir, installScriptFile)
	if err := os.WriteFile(path, []byte(installScript), 0o755); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
