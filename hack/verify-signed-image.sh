#!/bin/sh
set -eu

image=${IMG:-}
identity=${COSIGN_CERTIFICATE_IDENTITY:-}
cosign_bin=${COSIGN_BIN:-cosign}
issuer=https://token.actions.githubusercontent.com

case "$image" in
  *@sha256:*) ;;
  *) echo 'IMG must be repository@sha256:<64 lowercase hex>' >&2; exit 1 ;;
esac
repository=${image%@sha256:*}
digest=${image##*@sha256:}
case "$repository" in
  ''|*[!A-Za-z0-9._/:+-]*) echo 'IMG repository is invalid' >&2; exit 1 ;;
esac
if ! printf '%s\n' "$digest" | LC_ALL=C grep -Eq '^[0-9a-f]{64}$'; then
  echo 'IMG must be repository@sha256:<64 lowercase hex>' >&2
  exit 1
fi
if ! printf '%s\n' "$identity" | LC_ALL=C grep -Eq '^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml@refs/tags/v[0-9][A-Za-z0-9._-]*$'; then
  echo 'COSIGN_CERTIFICATE_IDENTITY must pin one GitHub release workflow at a version tag' >&2
  exit 1
fi
if [ ! -x "$cosign_bin" ] && ! command -v "$cosign_bin" >/dev/null 2>&1; then
  echo 'cosign is required to verify a production image' >&2
  exit 1
fi

"$cosign_bin" verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  "$image"
