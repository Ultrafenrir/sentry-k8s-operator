#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=${SENTRY_VERSION:-26.10.1}
cache_dir=${SENTRY_SELF_HOSTED_CACHE:-$root_dir/.cache/sentry-self-hosted}

case "$version" in
  24.7.1) port=${SENTRY_PORT:-24710} ;;
  26.10.1) port=${SENTRY_PORT:-26101} ;;
  *) echo "unsupported Sentry version: $version" >&2; exit 1 ;;
esac

case "$cache_dir" in
  /*) ;;
  *) echo 'SENTRY_SELF_HOSTED_CACHE must be an absolute path' >&2; exit 1 ;;
esac

for command in curl jq stat; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is missing: $command" >&2
    exit 1
  }
done

token_file="$cache_dir/tokens/$version.token"
if [ ! -f "$token_file" ]; then
  echo "managed local Sentry token is missing: run make sentry-bootstrap first" >&2
  exit 1
fi
if [ "$(stat -f '%Lp' "$token_file")" != 600 ]; then
  echo "managed local Sentry token must have mode 0600" >&2
  exit 1
fi

token=$(sed -n '1p' "$token_file")
if ! printf '%s\n' "$token" | grep -Eq '^sntryu_[0-9a-f]{64}$'; then
  echo "managed local Sentry token has an invalid format" >&2
  exit 1
fi

temporary_dir=$(mktemp -d /tmp/sko-sentry-live-api.XXXXXX)
cleanup() {
  unset token
  case "$temporary_dir" in
    /tmp/sko-sentry-live-api.*) rm -rf -- "$temporary_dir" ;;
    *) echo "refusing unexpected temporary path: $temporary_dir" >&2 ;;
  esac
}
trap cleanup EXIT INT TERM

auth_config="$temporary_dir/curl-auth.conf"
(umask 077; printf 'header = "Authorization: Bearer %s"\n' "$token" >"$auth_config")
unset token

base_url="http://127.0.0.1:$port"
version_slug=$(printf '%s' "$version" | tr '.' '-')
organization="sko-live-$version_slug"
team="sko-live"
project="sko-live-api"
managed_name='Live API [managed;o=acceptance;m=sko]'

request() {
  method=$1
  path=$2
  input=$3
  output=$4
  if [ -n "$input" ]; then
    status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 30 --request "$method" \
      --header 'Content-Type: application/json' --data-binary "@$input" \
      --output "$output" --write-out '%{http_code}' "$base_url$path")
  else
    status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 30 --request "$method" \
      --output "$output" --write-out '%{http_code}' "$base_url$path")
  fi
  case "$status" in
    2??) ;;
    *) echo "$method $path returned HTTP $status" >&2; exit 1 ;;
  esac
}

assert_json() {
  description=$1
  filter=$2
  file=$3
  shift 3
  if ! jq -e "$@" "$filter" "$file" >/dev/null; then
    echo "live API assertion failed: $description" >&2
    jq . "$file" >&2 || true
    exit 1
  fi
}

organization_response="$temporary_dir/organization-response.json"
request GET '/api/0/organizations/' '' "$organization_response"
assert_json "bootstrapped organization is listed" \
  'type == "array" and ([.[] | select(.slug == $slug and (.id | type == "string"))] | length) == 1' \
  "$organization_response" --arg slug "$organization"

team_response="$temporary_dir/team-response.json"
request GET "/api/0/organizations/$organization/teams/" '' "$team_response"
assert_json "bootstrapped team is listed" \
  'type == "array" and ([.[] | select(.slug == $slug and (.id | type == "string"))] | length) == 1' \
  "$team_response" --arg slug "$team"

project_request="$temporary_dir/project-request.json"
project_response="$temporary_dir/project-response.json"
jq -n --arg name "$managed_name" --arg slug "$project" \
  '{name: $name, slug: $slug, default_rules: false}' >"$project_request"
request POST "/api/0/teams/$organization/$team/projects/" "$project_request" "$project_response"
assert_json "created project preserves marker and omitted platform" \
  '.slug == $slug and .name == $name and (.id | type == "string") and ((.platform == null) or (.platform == ""))' \
  "$project_response" --arg slug "$project" --arg name "$managed_name"

project_get="$temporary_dir/project-get.json"
request GET "/api/0/projects/$organization/$project/" '' "$project_get"
assert_json "created project is readable by slug" \
  '.slug == $slug and .name == $name and (.id | type == "string")' \
  "$project_get" --arg slug "$project" --arg name "$managed_name"

keys_response="$temporary_dir/keys-response.json"
request GET "/api/0/projects/$organization/$project/keys/" '' "$keys_response"
assert_json "project has one active Default client key with a local DSN" '
  ([.[] | select(.isActive == true)] | length) == 1 and
  ([.[] | select(
    .isActive == true and
    ((.label // .name) == "Default") and
    (.dsn.public | type == "string") and
    (.dsn.public | test("^https?://[^/@]+@[^/]+/.+"))
  )] | length) == 1
' "$keys_response"

filters_response="$temporary_dir/filters-response.json"
request GET "/api/0/projects/$organization/$project/filters/" '' "$filters_response"
assert_json "published inbound filters are listed" '
  type == "array" and
  ([.[] | select(.id == "browser-extensions" and (.active | type == "boolean"))] | length) == 1 and
  ([.[] | select(.id == "localhost" and (.active | type == "boolean"))] | length) == 1 and
  ([.[] | select(.id == "web-crawlers" and (.active | type == "boolean"))] | length) == 1
' "$filters_response"

filter_request="$temporary_dir/filter-request.json"
filter_response="$temporary_dir/filter-response.json"
for filter_state in 'browser-extensions false' 'localhost true' 'web-crawlers false'; do
  filter_id=${filter_state% *}
  filter_active=${filter_state#* }
  jq -n --argjson active "$filter_active" '{active: $active}' >"$filter_request"
  request PUT "/api/0/projects/$organization/$project/filters/$filter_id/" \
    "$filter_request" "$filter_response"
done
request GET "/api/0/projects/$organization/$project/filters/" '' "$filters_response"
assert_json "inbound filter boolean updates round-trip" '
  ([.[] | select(.id == "browser-extensions" and .active == false)] | length) == 1 and
  ([.[] | select(.id == "localhost" and .active == true)] | length) == 1 and
  ([.[] | select(.id == "web-crawlers" and .active == false)] | length) == 1
' "$filters_response"

settings_request="$temporary_dir/settings-request.json"
settings_response="$temporary_dir/settings-response.json"
jq -n '{
  dataScrubber: true,
  dataScrubberDefaults: true,
  scrubIPAddresses: true,
  sensitiveFields: ["authorization", "password"],
  safeFields: ["request.headers.x-request-id"]
}' >"$settings_request"
request PUT "/api/0/projects/$organization/$project/" "$settings_request" "$settings_response"
request GET "/api/0/projects/$organization/$project/" '' "$settings_response"
assert_json "typed PII settings round-trip" '
  .dataScrubber == true and
  .dataScrubberDefaults == true and
  .scrubIPAddresses == true and
  .sensitiveFields == ["authorization", "password"] and
  .safeFields == ["request.headers.x-request-id"]
' "$settings_response"

if [ "$version" = 24.7.1 ]; then
  rules_response="$temporary_dir/rules-response.json"
  request GET "/api/0/projects/$organization/$project/rules/" '' "$rules_response"
  assert_json "default_rules=false creates no legacy issue-alert rules" \
    'type == "array" and length == 0' "$rules_response"
else
  # Sentry 26 publishes only POST on the project-detectors endpoint; GET is
  # intentionally forbidden even with alerts:read. The default_rules=false
  # effect remains covered by the source/contract test for this version.
  :
fi

delete_response="$temporary_dir/delete-response.json"
request DELETE "/api/0/projects/$organization/$project/" '' "$delete_response"

attempt=0
status=
while [ "$attempt" -lt 30 ]; do
  attempt=$((attempt + 1))
  status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 10 --output "$project_get" --write-out '%{http_code}' \
    "$base_url/api/0/projects/$organization/$project/")
  case "$status" in
    404) break ;;
    200) sleep 1 ;;
    *) echo "project delete verification returned HTTP $status" >&2; exit 1 ;;
  esac
done
if [ "$status" != 404 ]; then
  echo "project remained readable after delete" >&2
  exit 1
fi

echo "Sentry $version live API project/settings/key/delete acceptance passed"
