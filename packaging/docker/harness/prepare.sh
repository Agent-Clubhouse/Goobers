#!/bin/sh
set -eu

harness=${1:?harness required}
arch=${2:?target architecture required}
case "$harness/$arch" in
  copilot/amd64)
    package=@github/copilot-linux-x64
    version=1.0.80
    digest=aafd72b553700372032bb91c428c3e7c08a40faeede36f8043c5f8d9b2bfee8b9d362bf87eb55930ed328750d821e6d545c9848663ef6d53d8fe9b5550addc6c
    ;;
  copilot/arm64)
    package=@github/copilot-linux-arm64
    version=1.0.80
    digest=f285f037696ec871232284a4f009004c18d70e146842dba252f0bddc6a5f40a14723e2357a8395c08b00ab97ca3fc8cd085dab3ab60521c4e3c2646ddfc90271
    ;;
  claude/amd64)
    package=@anthropic-ai/claude-code-linux-x64
    version=2.1.263
    digest=d08aefa4b77fd053f469c430e7d4f80afc7faf89f0017281ad6d16ca1217ca6cd85aeacf08bced07e21cbbd32479ec00508fa9dcf4d7b127e68a5bff9fa5e501
    ;;
  claude/arm64)
    package=@anthropic-ai/claude-code-linux-arm64
    version=2.1.263
    digest=46526d2db97cc6a14c7f6cd474e283e28e4590d85c8068e54b7527f0f35f49bbf57cb420dd2a8142010984d12d49e2a512c3acdba467086fe3564e326a1f1477
    ;;
  *) echo 'Only copilot/claude on linux amd64/arm64 are supported' >&2; exit 1 ;;
esac

url="https://registry.npmjs.org/$package/-/${package##*/}-$version.tgz"
mkdir -p /out/package /out/bin
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 20 --max-time 300 --retry 2 --output /tmp/harness.tgz "$url"
printf '%s  /tmp/harness.tgz\n' "$digest" | sha512sum -c -
tar --extract --gzip --file /tmp/harness.tgz --directory /out/package \
  --strip-components=1 --no-same-owner --no-same-permissions
test -x "/out/package/$harness"
test -z "$(find /out/package -type f -perm /6000 -print)"
cp "/inputs/$harness.sh" "/out/bin/$harness"
chmod 755 "/out/bin/$harness"
printf '%s\n' "$version" > /out/version
printf '%s\nsha512:%s\n' "$url" "$digest" > /out/source
