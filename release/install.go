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
	printf 'Usage: install.sh vMAJOR.MINOR.PATCH [--guided [instance-path]]\n' >&2
	printf 'Installs the binary and documentation. Guided instance setup is opt-in\n' >&2
	printf 'via --guided; the default install never prompts or configures anything.\n' >&2
}

fail() {
	printf 'install: %s\n' "$*" >&2
	exit 1
}

if [ "${1:-}" = "--help" ] || [ "${1:-}" = "-h" ]; then
	usage
	exit 0
fi
if [ "$#" -lt 1 ]; then
	usage
	exit 2
fi

version=$1
shift
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

run_guided=0
instance_path=./goobers-instance
if [ "$#" -gt 0 ]; then
	case "$1" in
		--guided)
			run_guided=1
			shift
			if [ "$#" -gt 0 ]; then
				instance_path=$1
				shift
			fi
			;;
		*)
			usage
			fail "unexpected argument: $1 (guided setup is opt-in: pass --guided [instance-path])"
			;;
	esac
fi
if [ "$#" -gt 0 ]; then
	usage
	fail "too many arguments"
fi

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
docs_stage=
cleanup() {
	rm -rf "$tmp_dir"
	if [ -n "$docs_stage" ]; then
		rm -rf "$docs_stage"
	fi
}
trap cleanup 0
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
[ -f "${tmp_dir}/README.md" ] || fail "${archive} does not contain README.md"
[ -f "${tmp_dir}/docs/RELEASE.md" ] || fail "${archive} does not contain release documentation"
[ -f "${tmp_dir}/onboarding/manifest.json" ] || fail "${archive} does not contain onboarding assets"

if [ -n "${GOOBERS_INSTALL_DIR:-}" ]; then
	install_dir=$GOOBERS_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
	install_dir="${HOME}/.local/bin"
else
	fail "HOME is unset; set GOOBERS_INSTALL_DIR to choose an install directory"
fi

if [ -n "${GOOBERS_DOCS_DIR:-}" ]; then
	docs_root=$GOOBERS_DOCS_DIR
elif [ -n "${XDG_DATA_HOME:-}" ]; then
	docs_root="${XDG_DATA_HOME}/goobers"
elif [ -n "${HOME:-}" ]; then
	docs_root="${HOME}/.local/share/goobers"
else
	docs_root="${install_dir}/docs"
fi
docs_dir="${docs_root}/${version}"

installed_version=$("${tmp_dir}/goobers" --version | awk '{ print $2 }')
[ "$installed_version" = "$version" ] ||
	fail "archive binary did not report release ${version}"

mkdir -p "$install_dir" "$docs_root"
docs_stage="${docs_root}/.${version}.tmp.$$"
rm -rf "$docs_stage"
mkdir -p "${docs_stage}/docs" "${docs_stage}/onboarding"
install -m 0644 "${tmp_dir}/README.md" "${docs_stage}/README.md"
cp -R "${tmp_dir}/docs/." "${docs_stage}/docs/"
cp -R "${tmp_dir}/onboarding/." "${docs_stage}/onboarding/"
rm -rf "$docs_dir"
mv "$docs_stage" "$docs_dir"
docs_stage=

versioned_binary="${install_dir}/goobers-${version}"
install -m 0755 "${tmp_dir}/goobers" "$versioned_binary"
install -m 0755 "${tmp_dir}/goobers" "${install_dir}/goobers"
binary=$versioned_binary

printf 'Installed %s to %s\n' "$version" "$versioned_binary"
printf 'Updated current-version command at %s\n' "${install_dir}/goobers"
printf 'Installed %s documentation to %s\n' "$version" "$docs_dir"
printf 'Installed %s onboarding assets to %s\n' "$version" "${docs_dir}/onboarding"
case ":${PATH:-}:" in
	*":${install_dir}:"*) ;;
	*) printf 'Add %s to PATH before opening a new shell.\n' "$install_dir" ;;
esac
if [ "$run_guided" = 1 ]; then
	printf 'Starting guided setup for %s...\n\n' "$instance_path"
	if "$binary" init --guided "$instance_path"; then
		printf '\nGuided setup completed for %s\n' "$instance_path"
	else
		setup_status=$?
		printf '\ninstall: the binary and documentation installed successfully; guided setup exited with status %s\n' "$setup_status" >&2
		printf 'Re-run guided setup any time with: %s init --guided %s\n' "$binary" "$instance_path" >&2
		exit "$setup_status"
	fi
else
	printf '\nNext steps (see %s/docs/guides/quickstart.md):\n' "$docs_dir"
	printf '  Credential-free tour:   %s init --demo ./demo-instance && %s run demo ./demo-instance\n' "$binary" "$binary"
	printf '  Set up your repository: %s init --guided ./my-instance\n' "$binary"
fi
`

func writeInstallScript(outDir string) (string, error) {
	path := filepath.Join(outDir, installScriptFile)
	if err := os.WriteFile(path, []byte(installScript), 0o755); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
