#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
action=${1:?action must be metadata, prepare, verify, up, bootstrap, smoke, status, logs, down, reset, or clean}
version=${SENTRY_VERSION:-24.7.1}
profile=${SENTRY_PROFILE:-feature-complete}
wait_timeout=${SENTRY_WAIT_TIMEOUT:-1800}
cache_input=${SENTRY_SELF_HOSTED_CACHE:-$root_dir/.cache/sentry-self-hosted}
upstream=https://github.com/getsentry/self-hosted.git
temporary_checkout=
temporary_token=

case "$version" in
  24.7.1)
    expected_commit=f330f3040d3ab81055f92b4ac4265fbbfe17487c
    version_slug=24-7-1
    default_port=24710
    persistent_suffixes='clickhouse data kafka postgres redis symbolicator zookeeper'
    min_feature_memory_mb=3800
    min_errors_memory_mb=3800
    min_feature_cpus=2
    min_errors_cpus=2
    ;;
  26.10.1)
    expected_commit=192a826421decd7f8ce6673bb9a299737d0d6916
    version_slug=26-10-1
    default_port=26101
    persistent_suffixes='clickhouse data kafka postgres redis seaweedfs'
    min_feature_memory_mb=14000
    min_errors_memory_mb=7000
    min_feature_cpus=4
    min_errors_cpus=2
    ;;
  *)
    echo "unsupported SENTRY_VERSION: $version (allowed: 24.7.1, 26.10.1)" >&2
    exit 1
    ;;
esac

case "$profile" in
  feature-complete|errors-only) ;;
  *) echo 'SENTRY_PROFILE must be feature-complete or errors-only' >&2; exit 1 ;;
esac
case "$wait_timeout" in
  ''|*[!0-9]*) echo 'SENTRY_WAIT_TIMEOUT must be a positive number of seconds' >&2; exit 1 ;;
esac
if [ "$wait_timeout" -lt 60 ] || [ "$wait_timeout" -gt 7200 ]; then
  echo 'SENTRY_WAIT_TIMEOUT must be between 60 and 7200 seconds' >&2
  exit 1
fi

port=${SENTRY_PORT:-$default_port}
case "$port" in
  ''|*[!0-9]*) echo 'SENTRY_PORT must be numeric' >&2; exit 1 ;;
esac
if [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; then
  echo 'SENTRY_PORT must be between 1024 and 65535' >&2
  exit 1
fi

project="sko-sentry-$version_slug"
volume_prefix=$project
if [ "$version" = 24.7.1 ]; then
  other_project=sko-sentry-26-10-1
else
  other_project=sko-sentry-24-7-1
fi

require_command() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1" >&2; exit 1; }
}

select_bash() {
  if [ -n "${SENTRY_BASH_BIN:-}" ]; then
    case "$SENTRY_BASH_BIN" in
      /*) candidates=$SENTRY_BASH_BIN ;;
      *) echo 'SENTRY_BASH_BIN must be an absolute path' >&2; exit 1 ;;
    esac
  else
    candidates="/opt/homebrew/bin/bash /usr/local/bin/bash $(command -v bash 2>/dev/null || true)"
  fi
  for candidate in $candidates; do
    if [ ! -x "$candidate" ]; then continue; fi
    bash_version=$($candidate -c 'printf "%s %s\n" "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}"')
    bash_major=${bash_version% *}
    bash_minor=${bash_version#* }
    if [ "$bash_major" -gt 4 ] || { [ "$bash_major" -eq 4 ] && [ "$bash_minor" -ge 4 ]; }; then
      bash_bin=$candidate
      bash_dir=$(dirname -- "$bash_bin")
      return 0
    fi
  done
  echo 'Sentry installer requires Bash >=4.4; install Homebrew bash or set SENTRY_BASH_BIN' >&2
  exit 1
}

initialize_paths() {
  case "$cache_input" in
    /*) ;;
    *) echo 'SENTRY_SELF_HOSTED_CACHE must be an absolute path' >&2; exit 1 ;;
  esac
  case "$cache_input" in
    /|/tmp|/var|/Users|/home) echo "refusing broad cache path: $cache_input" >&2; exit 1 ;;
  esac
  mkdir -p "$cache_input"
  cache_dir=$(CDPATH= cd -- "$cache_input" && pwd -P)
  work_dir="$cache_dir/$version"
  marker="$work_dir/.sko-managed"
  token_dir="$cache_dir/tokens"
  token_file="$token_dir/$version.token"
}

expected_marker() {
  printf 'version=%s\ncommit=%s\nproject=%s\nvolume_prefix=%s\n' \
    "$version" "$expected_commit" "$project" "$volume_prefix"
}

is_managed_checkout() {
  [ -d "$work_dir" ] && [ ! -L "$work_dir" ] && [ -f "$marker" ] && \
    [ "$(cat "$marker")" = "$(expected_marker)" ]
}

cleanup_temporary_checkout() {
  if [ -n "$temporary_checkout" ]; then
    case "$temporary_checkout" in
      "$cache_dir"/.*.tmp.*) rm -rf -- "$temporary_checkout" ;;
      *) echo "refusing to clean unexpected temporary path: $temporary_checkout" >&2 ;;
    esac
  fi
  if [ -n "$temporary_token" ]; then
    case "$temporary_token" in
      "$token_dir"/.*.tmp.*) rm -f -- "$temporary_token" ;;
      *) echo "refusing to clean unexpected temporary token: $temporary_token" >&2 ;;
    esac
  fi
}
trap cleanup_temporary_checkout EXIT INT TERM

replace_token() {
  tree=$1
  from=$2
  to=$3
  files=$tree/docker-compose.yml
  if [ -d "$tree/install" ]; then
    install_files=$(grep -rl -- "$from" "$tree/install" 2>/dev/null || true)
    if [ -n "$install_files" ]; then
      files="$files $install_files"
    fi
  fi
  FROM_TOKEN="$from" TO_TOKEN="$to" perl -pi -e \
    's/\Q$ENV{FROM_TOKEN}\E/$ENV{TO_TOKEN}/g' $files
}

set_env_value() {
  env_file=$1
  key=$2
  value=$3
  if grep -q "^$key=" "$env_file"; then
    ENV_KEY="$key" ENV_VALUE="$value" perl -pi -e \
      's/^\Q$ENV{ENV_KEY}\E=.*/$ENV{ENV_KEY}=$ENV{ENV_VALUE}/' "$env_file"
  else
    printf '%s=%s\n' "$key" "$value" >>"$env_file"
  fi
}

prepare_checkout() {
  require_command git
  require_command perl
  initialize_paths
  if [ -e "$work_dir" ]; then
    if ! is_managed_checkout; then
      echo "refusing to replace unmanaged checkout: $work_dir" >&2
      exit 1
    fi
    case "$work_dir" in
      "$cache_dir"/24.7.1|"$cache_dir"/26.10.1) rm -rf -- "$work_dir" ;;
      *) echo "refusing to replace unexpected checkout path: $work_dir" >&2; exit 1 ;;
    esac
  fi

  temporary_checkout="$cache_dir/.$version.tmp.$$"
  if [ -e "$temporary_checkout" ]; then
    echo "refusing existing temporary checkout: $temporary_checkout" >&2
    exit 1
  fi
  git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$version" \
    "$upstream" "$temporary_checkout"
  actual_commit=$(git -C "$temporary_checkout" rev-parse HEAD)
  if [ "$actual_commit" != "$expected_commit" ]; then
    echo "self-hosted tag $version resolved to unexpected commit $actual_commit" >&2
    exit 1
  fi

  for suffix in $persistent_suffixes; do
    replace_token "$temporary_checkout" "sentry-$suffix" "$volume_prefix-$suffix"
  done
  # Recent self-hosted releases force the buildx builder named "default". Docker
  # Desktop normally uses the builder matching its active context (for example,
  # "desktop-linux"), and buildx refuses to use a builder from another context.
  # Let Compose select the builder for the current context instead.
  replace_token "$temporary_checkout" 'export BUILDX_BUILDER=default' 'unset BUILDX_BUILDER'
  set_env_value "$temporary_checkout/.env" COMPOSE_PROJECT_NAME "$project"
  set_env_value "$temporary_checkout/.env" COMPOSE_PROFILES "$profile"
  set_env_value "$temporary_checkout/.env" SENTRY_BIND "127.0.0.1:$port"
  set_env_value "$temporary_checkout/.env" SENTRY_EVENT_RETENTION_DAYS 1
  expected_marker >"$temporary_checkout/.sko-managed"
  mv "$temporary_checkout" "$work_dir"
  temporary_checkout=
}

compose() {
  (cd "$work_dir" && docker compose --project-name "$project" --profile "$profile" "$@")
}

remove_labeled_resources() {
  ids=$(docker ps -aq --filter "label=com.docker.compose.project=$project")
  if [ -n "$ids" ]; then
    for id in $ids; do docker rm -f "$id" >/dev/null; done
  fi
  networks=$(docker network ls -q --filter "label=com.docker.compose.project=$project")
  if [ -n "$networks" ]; then
    for network in $networks; do docker network rm "$network" >/dev/null; done
  fi
  volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project")
  if [ -n "$volumes" ]; then
    for volume in $volumes; do docker volume rm "$volume" >/dev/null; done
  fi
}

remove_external_volumes() {
  for suffix in $persistent_suffixes postgres-new; do
    volume="$volume_prefix-$suffix"
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      docker volume rm "$volume" >/dev/null
    fi
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      echo "failed to remove managed volume: $volume" >&2
      exit 1
    fi
  done
}

remove_managed_token() {
  case "$token_file" in
    "$cache_dir"/tokens/24.7.1.token|"$cache_dir"/tokens/26.10.1.token)
      rm -f -- "$token_file"
      ;;
    *) echo "refusing unexpected managed token path: $token_file" >&2; exit 1 ;;
  esac
}

reset_runtime() {
  require_command docker
  initialize_paths
  if is_managed_checkout; then
    compose down --volumes --remove-orphans --timeout 30 >/dev/null 2>&1 || true
  elif [ -e "$work_dir" ]; then
    echo "refusing Compose operations through unmanaged checkout: $work_dir" >&2
    exit 1
  fi
  remove_labeled_resources
  remove_external_volumes
  remove_managed_token
  if [ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$project")" ]; then
    echo "managed containers remain for project $project" >&2
    exit 1
  fi
  if [ -n "$(docker volume ls -q --filter "label=com.docker.compose.project=$project")" ]; then
    echo "managed Compose volumes remain for project $project" >&2
    exit 1
  fi
}

preflight_resources() {
  require_command docker
  info=$(docker info --format '{{.MemTotal}}|{{.NCPU}}')
  memory_bytes=${info%|*}
  cpus=${info#*|}
  memory_mb=$((memory_bytes / 1048576))
  if [ "$profile" = feature-complete ]; then
    required_memory_mb=$min_feature_memory_mb
    required_cpus=$min_feature_cpus
  else
    required_memory_mb=$min_errors_memory_mb
    required_cpus=$min_errors_cpus
  fi
  if [ "$memory_mb" -lt "$required_memory_mb" ] || [ "$cpus" -lt "$required_cpus" ]; then
    echo "Docker resources are insufficient for Sentry $version/$profile: have ${memory_mb}MB/${cpus}CPU, require ${required_memory_mb}MB/${required_cpus}CPU" >&2
    exit 1
  fi
}

verify_checkout() {
  require_command docker
  initialize_paths
  if ! is_managed_checkout; then
    echo "managed checkout is missing: run prepare first" >&2
    exit 1
  fi
  rendered=$(mktemp /tmp/sko-sentry-compose.XXXXXX)
  cleanup_rendered() {
    case "$rendered" in /tmp/sko-sentry-compose.*) rm -f -- "$rendered" ;; esac
  }
  trap 'cleanup_rendered; cleanup_temporary_checkout' EXIT INT TERM
  compose config >"$rendered"
  grep -Eq "^SENTRY_IMAGE=(ghcr.io/)?getsentry/sentry:$version$" "$work_dir/.env"
  for suffix in $persistent_suffixes; do
    grep -q "name: $volume_prefix-$suffix" "$rendered"
  done
  if grep -Eq 'name: sentry-(clickhouse|data|kafka|postgres|redis|seaweedfs|symbolicator|zookeeper)$' "$rendered"; then
    echo 'rendered Compose still references an unscoped external Sentry volume' >&2
    exit 1
  fi
  cleanup_rendered
  trap cleanup_temporary_checkout EXIT INT TERM
}

wait_http() {
  require_command curl
  attempts=$((wait_timeout / 2))
  attempt=0
  while [ "$attempt" -lt "$attempts" ]; do
    attempt=$((attempt + 1))
    if curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
      "http://127.0.0.1:$port/_health/" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "Sentry health endpoint did not become ready within ${wait_timeout}s" >&2
  return 1
}

accept_optional_geoip_exit() {
  runtime_states=$(docker ps -a --filter "label=com.docker.compose.project=$project" \
    --format '{{.Label "com.docker.compose.service"}}|{{.State}}|{{.Status}}')
  geoip_state=$(printf '%s\n' "$runtime_states" | awk -F '|' '$1 == "geoipupdate" { print; exit }')
  case "$geoip_state" in
    'geoipupdate|exited|Exited (1)'*) ;;
    *) return 1 ;;
  esac
  unexpected=$(printf '%s\n' "$runtime_states" | awk -F '|' '
    $1 != "geoipupdate" &&
      ($2 != "running" || $3 ~ /\(unhealthy\)|\(health: starting\)|Restarting/) { print }
  ')
  if [ -n "$unexpected" ]; then
    echo 'Sentry services failed to become ready:' >&2
    printf '%s\n' "$unexpected" >&2
    return 1
  fi
  echo 'Ignoring the expected optional geoipupdate exit: no MaxMind credentials are configured'
}

bootstrap_access() {
  require_command curl
  require_command docker
  initialize_paths
  if ! is_managed_checkout; then
    echo 'managed checkout is missing' >&2
    exit 1
  fi
  if [ -z "$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter 'label=com.docker.compose.service=web')" ]; then
    echo "Sentry $version web service is not running" >&2
    exit 1
  fi

  bootstrap_email=sko-local-acceptance@example.invalid
  compose run --rm -T web createuser --email "$bootstrap_email" --no-password \
    --superuser --staff --no-input --force-update >/dev/null

  if [ "$version" = 24.7.1 ]; then
    user_import='from sentry.models.user import User'
  else
    user_import='from sentry.users.models.user import User'
  fi
  python_code="$user_import; from django.conf import settings; from sentry.models.apitoken import ApiToken; from sentry.types.token import AuthTokenType; user=User.objects.get(username='$bootstrap_email'); desired={'org:read','org:write','org:admin','org:integrations','member:read','team:read','team:write','team:admin','project:read','project:write','project:admin','alerts:read','alerts:write'}; missing=desired-set(settings.SENTRY_SCOPES); assert not missing, 'required acceptance scopes unavailable'; [item.delete() for item in ApiToken.objects.filter(user=user, application__isnull=True, name='sko-local-acceptance')]; created=ApiToken.objects.create(user=user, name='sko-local-acceptance', token_type=AuthTokenType.USER, scope_list=sorted(desired), expires_at=None); print('SKO_TOKEN='+created.plaintext_token)"
  if ! token_output=$(compose run --rm -T web shell -c "$python_code" 2>/dev/null); then
    echo "failed to create local Sentry API token for $version" >&2
    exit 1
  fi
  token=$(printf '%s\n' "$token_output" | sed -n 's/^SKO_TOKEN=//p' | tail -n 1)
  if ! printf '%s\n' "$token" | grep -Eq '^sntryu_[0-9a-f]{64}$'; then
    echo "Sentry $version returned an invalid local API token" >&2
    exit 1
  fi

  mkdir -p "$token_dir"
  chmod 700 "$token_dir"
  temporary_token="$token_dir/.$version.token.tmp.$$"
  (umask 077; printf '%s\n' "$token" >"$temporary_token")
  chmod 600 "$temporary_token"
  mv -f -- "$temporary_token" "$token_file"
  temporary_token=

  if ! printf 'header = "Authorization: Bearer %s"\n' "$token" | \
    curl --config - --silent --show-error --fail --noproxy '*' --connect-timeout 2 \
      --max-time 30 "http://127.0.0.1:$port/api/0/organizations/" >/dev/null; then
    remove_managed_token
    echo "local Sentry API token authentication failed for $version" >&2
    exit 1
  fi
  unset token token_output python_code
  echo "Local Sentry $version API access is ready; token stored with mode 0600 at $token_file"
}

show_metadata() {
  printf 'version=%s\ncommit=%s\nprofile=%s\nproject=%s\nport=%s\nvolume_prefix=%s\n' \
    "$version" "$expected_commit" "$profile" "$project" "$port" "$volume_prefix"
}

case "$action" in
  metadata)
    show_metadata
    ;;
  prepare)
    prepare_checkout
    verify_checkout
    show_metadata
    ;;
  verify)
    prepare_checkout
    verify_checkout
    echo "Sentry self-hosted $version/$profile official Compose is pinned and all persistent volumes are scoped to $volume_prefix"
    ;;
  up)
    require_command curl
    require_command docker
    select_bash
    docker compose version >/dev/null
    if [ -n "$(docker ps -q --filter "label=com.docker.compose.project=$other_project")" ]; then
      echo "refusing concurrent Sentry versions: stop project $other_project first" >&2
      exit 1
    fi
    reset_runtime
    prepare_checkout
    verify_checkout
    preflight_resources
    install_args='--skip-commit-check --skip-user-creation --no-report-self-hosted-issues'
    if [ "$version" = 26.10.1 ]; then
      install_args="$install_args --apply-automatic-config-updates"
    fi
    (cd "$work_dir" && PATH="$bash_dir:$PATH" REPORT_SELF_HOSTED_ISSUES=0 SKIP_USER_CREATION=1 \
      "$bash_bin" ./install.sh $install_args)
    if ! compose up --detach --wait --wait-timeout "$wait_timeout"; then
      accept_optional_geoip_exit
    fi
    wait_http
    echo "Sentry self-hosted $version/$profile is ready at http://127.0.0.1:$port with fresh version-scoped volumes"
    ;;
  bootstrap)
    bootstrap_access
    ;;
  smoke)
    initialize_paths
    wait_http
    echo "Sentry self-hosted $version responds on /_health/"
    ;;
  status)
    require_command docker
    initialize_paths
    show_metadata
    docker ps -a --filter "label=com.docker.compose.project=$project" \
      --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
    printf '%s\n' 'managed volumes:'
    docker volume ls --filter "label=com.docker.compose.project=$project" --format '{{.Name}}'
    for suffix in $persistent_suffixes; do
      volume="$volume_prefix-$suffix"
      docker volume inspect "$volume" --format '{{.Name}}' 2>/dev/null || true
    done
    ;;
  logs)
    require_command docker
    initialize_paths
    if ! is_managed_checkout; then echo 'managed checkout is missing' >&2; exit 1; fi
    compose logs --tail "${SENTRY_LOG_TAIL:-200}"
    ;;
  down|reset)
    reset_runtime
    echo "Sentry self-hosted $version runtime and all managed volumes were removed"
    ;;
  clean)
    reset_runtime
    if [ -e "$work_dir" ]; then
      if ! is_managed_checkout; then
        echo "refusing to remove unmanaged checkout: $work_dir" >&2
        exit 1
      fi
      case "$work_dir" in
        "$cache_dir"/24.7.1|"$cache_dir"/26.10.1) rm -rf -- "$work_dir" ;;
        *) echo "refusing unexpected checkout path: $work_dir" >&2; exit 1 ;;
      esac
    fi
    echo "Sentry self-hosted $version runtime, volumes and managed checkout were removed"
    ;;
  *)
    echo "unsupported action: $action" >&2
    exit 1
    ;;
esac
