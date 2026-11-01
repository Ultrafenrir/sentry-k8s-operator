#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
checker=$root_dir/hack/check-docker-credentials.sh
makefile=$root_dir/Makefile

sh -n "$checker"

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/sko-verify-docker-credential.XXXXXX")
cleanup() {
  rm -rf "$temporary_dir"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$temporary_dir/bin" "$temporary_dir/config"
cat >"$temporary_dir/bin/docker" <<'EOF'
#!/bin/sh
if [ "${1:-}" = context ] && [ "${2:-}" = show ]; then
  printf '%s\n' desktop-linux
  exit 0
fi
exit 1
EOF
chmod 755 "$temporary_dir/bin/docker"

printf '%s\n' '{"credsStore":"missing-test-helper","auths":{"registry.example.invalid":{"auth":"must-not-be-read"}}}' >"$temporary_dir/config/config.json"
if PATH="$temporary_dir/bin:$PATH" DOCKER_CONFIG="$temporary_dir/config" CONTAINER_TOOL=docker sh "$checker" check >"$temporary_dir/output" 2>"$temporary_dir/error"; then
  echo 'missing configured Docker credential helper unexpectedly passed' >&2
  exit 1
fi
grep -q 'docker-credential-missing-test-helper' "$temporary_dir/error"
if grep -q 'must-not-be-read' "$temporary_dir/output" "$temporary_dir/error"; then
  echo 'Docker credential check exposed credential configuration data' >&2
  exit 1
fi

cat >"$temporary_dir/bin/docker-credential-test-helper" <<'EOF'
#!/bin/sh
[ "${1:-}" = list ] || exit 1
printf '%s\n' '{"registry.example.invalid":"redacted-user"}'
EOF
chmod 755 "$temporary_dir/bin/docker-credential-test-helper"
printf '%s\n' '{"credHelpers":{"registry.example.invalid":"test-helper"}}' >"$temporary_dir/config/config.json"
PATH="$temporary_dir/bin:$PATH" DOCKER_CONFIG="$temporary_dir/config" CONTAINER_TOOL=docker sh "$checker" check >"$temporary_dir/output"
grep -q 'configured helpers=test-helper' "$temporary_dir/output"
PATH="$temporary_dir/bin:$PATH" DOCKER_CONFIG="$temporary_dir/config" CONTAINER_TOOL=docker DOCKER_CREDENTIAL_PROBE_TIMEOUT=1 sh "$checker" probe >"$temporary_dir/output"
grep -q 'Docker credential probe passed' "$temporary_dir/output"
if grep -q 'redacted-user' "$temporary_dir/output"; then
  echo 'Docker credential probe exposed helper output' >&2
  exit 1
fi

cat >"$temporary_dir/bin/docker-credential-test-helper" <<'EOF'
#!/bin/sh
sleep 5
EOF
chmod 755 "$temporary_dir/bin/docker-credential-test-helper"
if PATH="$temporary_dir/bin:$PATH" DOCKER_CONFIG="$temporary_dir/config" CONTAINER_TOOL=docker DOCKER_CREDENTIAL_PROBE_TIMEOUT=1 sh "$checker" probe >"$temporary_dir/output" 2>"$temporary_dir/error"; then
  echo 'hanging Docker credential helper unexpectedly passed' >&2
  exit 1
fi
grep -q 'did not answer within 1s' "$temporary_dir/error"

grep -q '^docker-build: docker-credential-preflight' "$makefile"
grep -q '^docker-push: docker-credential-preflight' "$makefile"
grep -q '^docker-buildx: verify-release-tag verify-base-image-digests docker-credential-preflight' "$makefile"

echo 'Fail-closed Docker credential diagnostics verified'
