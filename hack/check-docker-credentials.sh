#!/bin/sh
set -eu

action=${1:-check}
container_tool=${CONTAINER_TOOL:-docker}
probe_timeout=${DOCKER_CREDENTIAL_PROBE_TIMEOUT:-5}

case "$action" in
  check|probe) ;;
  *) echo 'usage: check-docker-credentials.sh [check|probe]' >&2; exit 2 ;;
esac

case "$probe_timeout" in
  ''|*[!0-9]*) echo 'DOCKER_CREDENTIAL_PROBE_TIMEOUT must be an integer from 1 through 30' >&2; exit 2 ;;
esac
if [ "$probe_timeout" -lt 1 ] || [ "$probe_timeout" -gt 30 ]; then
  echo 'DOCKER_CREDENTIAL_PROBE_TIMEOUT must be an integer from 1 through 30' >&2
  exit 2
fi

tool_name=${container_tool##*/}
if [ "$tool_name" != docker ]; then
  echo "Docker credential check skipped for container tool '$tool_name'"
  exit 0
fi

if ! command -v "$container_tool" >/dev/null 2>&1; then
  echo "Docker executable is unavailable: $container_tool" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo 'jq is required for the Docker credential check' >&2
  exit 1
fi

docker_config_root=${DOCKER_CONFIG:-${HOME:?HOME is required when DOCKER_CONFIG is unset}/.docker}
docker_config=$docker_config_root/config.json
context=$($container_tool context show 2>/dev/null) || {
  echo 'Docker context lookup failed; start Docker Desktop and verify the selected context' >&2
  exit 1
}
if [ -z "$context" ]; then
  echo 'Docker returned an empty active context' >&2
  exit 1
fi

if [ ! -e "$docker_config" ]; then
  echo "Docker credential preflight passed: context=$context; configured helpers=none"
  exit 0
fi
if [ ! -f "$docker_config" ] || [ ! -r "$docker_config" ]; then
  echo 'Docker config exists but is not a readable regular file' >&2
  exit 1
fi
if ! jq -e 'type == "object"' "$docker_config" >/dev/null 2>&1; then
  echo 'Docker config is not valid JSON object data' >&2
  exit 1
fi

helpers=$(jq -r '[.credsStore?, ((.credHelpers? // {}) | to_entries[] | .value)] | map(select(type == "string" and length > 0)) | unique[]' "$docker_config")
if [ -z "$helpers" ]; then
  echo "Docker credential preflight passed: context=$context; configured helpers=none"
  exit 0
fi

helper_summary=
for helper in $helpers; do
  case "$helper" in
    *[!A-Za-z0-9._-]*|'')
      echo 'Docker config contains an unsafe credential-helper name' >&2
      exit 1
      ;;
  esac
  helper_command=docker-credential-$helper
  if ! command -v "$helper_command" >/dev/null 2>&1; then
    echo "Docker credential helper is configured but unavailable: $helper_command" >&2
    echo 'Repair Docker Desktop credential integration or select an intentional clean Docker config; the build will not bypass credential handling' >&2
    exit 1
  fi
  if [ -n "$helper_summary" ]; then
    helper_summary="$helper_summary,$helper"
  else
    helper_summary=$helper
  fi
done

if [ "$action" = check ]; then
  echo "Docker credential preflight passed: context=$context; configured helpers=$helper_summary"
  exit 0
fi

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/sko-docker-credential.XXXXXX")
cleanup() {
  rm -f "$temporary_dir/timed-out"
  rmdir "$temporary_dir" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

for helper in $helpers; do
  helper_command=docker-credential-$helper
  "$helper_command" list </dev/null >/dev/null 2>&1 &
  helper_pid=$!
  (
    sleep "$probe_timeout"
    if kill -0 "$helper_pid" 2>/dev/null; then
      : >"$temporary_dir/timed-out"
      kill -TERM "$helper_pid" 2>/dev/null || true
    fi
  ) &
  watcher_pid=$!
  probe_status=0
  wait "$helper_pid" || probe_status=$?
  kill "$watcher_pid" 2>/dev/null || true
  wait "$watcher_pid" 2>/dev/null || true
  if [ -f "$temporary_dir/timed-out" ]; then
    echo "Docker credential helper did not answer within ${probe_timeout}s: $helper_command" >&2
    echo 'Repair or unlock the configured helper before retrying the image build' >&2
    exit 1
  fi
  if [ "$probe_status" -ne 0 ]; then
    echo "Docker credential helper probe failed without exposing helper output: $helper_command (exit $probe_status)" >&2
    exit 1
  fi
done

echo "Docker credential probe passed: context=$context; configured helpers=$helper_summary"
