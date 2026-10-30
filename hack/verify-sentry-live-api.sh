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

public_key=$(jq -r '[.[] | select(.isActive == true)][0].public // empty' "$keys_response")
project_id=$(jq -r '[.[] | select(.isActive == true)][0].projectId // empty' "$keys_response")
if ! printf '%s\n' "$public_key" | grep -Eq '^[0-9a-f]{32}$'; then
  echo 'active project key has an invalid public key' >&2
  exit 1
fi
if ! printf '%s\n' "$project_id" | grep -Eq '^[0-9]+$'; then
  echo 'active project key has an invalid project identity' >&2
  exit 1
fi

case "$version" in
  24.7.1) event_id=24710000000000000000000000000001 ;;
  26.10.1) event_id=26101000000000000000000000000001 ;;
esac
event_request="$temporary_dir/event-request.json"
event_response="$temporary_dir/event-response.json"
jq -n --arg event_id "$event_id" '{
  event_id: $event_id,
  platform: "other",
  logger: "sko-live",
  level: "error",
  message: "Synthetic PII acceptance",
  environment: "sko-live",
  extra: {
    authorization: "Bearer sko-sensitive-authorization",
    password: "sko-sensitive-password",
    request_id: "sko-safe-request-id"
  }
}' >"$event_request"
ingest_status=$(curl --silent --show-error --noproxy '*' \
  --connect-timeout 2 --max-time 30 --request POST \
  --header 'Content-Type: application/json' --data-binary "@$event_request" \
  --output "$event_response" --write-out '%{http_code}' \
  "$base_url/api/$project_id/store/?sentry_key=$public_key&sentry_version=7")
case "$ingest_status" in
  2??) ;;
  *) echo "synthetic event ingestion returned HTTP $ingest_status" >&2; exit 1 ;;
esac

event_attempt=0
event_status=
while [ "$event_attempt" -lt 120 ]; do
  event_attempt=$((event_attempt + 1))
  event_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 10 --output "$event_response" --write-out '%{http_code}' \
    "$base_url/api/0/projects/$organization/$project/events/$event_id/")
  case "$event_status" in
    200) break ;;
    404) sleep 1 ;;
    *) echo "synthetic event lookup returned HTTP $event_status" >&2; exit 1 ;;
  esac
done
if [ "$event_status" != 200 ]; then
  echo 'synthetic event was not readable after ingestion' >&2
  exit 1
fi
assert_json "ingested event is returned by exact identity with sensitive extras scrubbed" '
  .eventID == $event_id and
  .message == "Synthetic PII acceptance" and
  .context.authorization == "[Filtered]" and
  .context.password == "[Filtered]" and
  .context.request_id == "sko-safe-request-id" and
  (tostring | contains("sko-sensitive-authorization") | not) and
  (tostring | contains("sko-sensitive-password") | not)
' "$event_response" --arg event_id "$event_id"

if [ "$version" = 24.7.1 ]; then
  rules_response="$temporary_dir/rules-response.json"
  request GET "/api/0/projects/$organization/$project/rules/" '' "$rules_response"
  assert_json "default_rules=false creates no legacy issue-alert rules" \
    'type == "array" and length == 0' "$rules_response"

  rule_request="$temporary_dir/rule-request.json"
  rule_response="$temporary_dir/rule-response.json"
  rule_name='Live first seen [managed;o=acceptance;m=sko]'
  jq -n --arg name "$rule_name" '{
    name: $name,
    actionMatch: "all",
    filterMatch: "all",
    conditions: [{id: "sentry.rules.conditions.first_seen_event.FirstSeenEventCondition"}],
    filters: [],
    actions: [{id: "sentry.rules.actions.notify_event.NotifyEventAction"}],
    frequency: 30
  }' >"$rule_request"
  request POST "/api/0/projects/$organization/$project/rules/" \
    "$rule_request" "$rule_response"
  assert_json "legacy issue alert is created with its ownership marker" \
    '.name == $name and (.id | tostring | test("^[0-9]+$"))' \
    "$rule_response" --arg name "$rule_name"
  rule_id=$(jq -r '.id | tostring' "$rule_response")

  request GET "/api/0/projects/$organization/$project/rules/$rule_id/" '' "$rule_response"
  assert_json "legacy issue alert is readable by exact identity" \
    '(.id | tostring) == $id and .name == $name and .frequency == 30' \
    "$rule_response" --arg id "$rule_id" --arg name "$rule_name"

  updated_rule_name='Live first seen updated [managed;o=acceptance;m=sko]'
  jq -n --arg name "$updated_rule_name" '{
    name: $name,
    actionMatch: "all",
    filterMatch: "all",
    conditions: [{id: "sentry.rules.conditions.first_seen_event.FirstSeenEventCondition"}],
    filters: [],
    actions: [{id: "sentry.rules.actions.notify_event.NotifyEventAction"}],
    frequency: 60
  }' >"$rule_request"
  request PUT "/api/0/projects/$organization/$project/rules/$rule_id/" \
    "$rule_request" "$rule_response"
  request GET "/api/0/projects/$organization/$project/rules/$rule_id/" '' "$rule_response"
  assert_json "legacy issue alert update round-trips without changing identity" \
    '(.id | tostring) == $id and .name == $name and .frequency == 60' \
    "$rule_response" --arg id "$rule_id" --arg name "$updated_rule_name"

  request DELETE "/api/0/projects/$organization/$project/rules/$rule_id/" '' "$rule_response"
  request GET "/api/0/projects/$organization/$project/rules/$rule_id/" '' "$rule_response"
  assert_json "legacy issue alert delete marks the exact rule inactive" \
    '(.id | tostring) == $id and .status == "disabled"' \
    "$rule_response" --arg id "$rule_id"
  request GET "/api/0/projects/$organization/$project/rules/" '' "$rules_response"
  assert_json "deleted legacy issue alert is excluded from the active list" \
    'type == "array" and ([.[] | select((.id | tostring) == $id)] | length) == 0' \
    "$rules_response" --arg id "$rule_id"

  metric_request="$temporary_dir/metric-request.json"
  metric_response="$temporary_dir/metric-response.json"
  metric_name='Live event count [managed;o=acceptance;m=sko]'
  jq -n --arg name "$metric_name" --arg project "$project" '{
    name: $name,
    aggregate: "count()",
    query: "",
    timeWindow: "300",
    resolveThreshold: 100,
    thresholdType: 0,
    triggers: [{label: "critical", alertThreshold: 200, actions: []}],
    projects: [$project],
    activations: []
  }' >"$metric_request"
  request POST "/api/0/organizations/$organization/alert-rules/" \
    "$metric_request" "$metric_response"
  assert_json "legacy metric alert is created with its ownership marker" \
    '.name == $name and (.id | tostring | test("^[0-9]+$"))' \
    "$metric_response" --arg name "$metric_name"
  metric_id=$(jq -r '.id | tostring' "$metric_response")

  request GET "/api/0/organizations/$organization/alert-rules/$metric_id/" '' \
    "$metric_response"
  assert_json "legacy metric alert is readable by exact identity" \
    '(.id | tostring) == $id and .name == $name and .aggregate == "count()"' \
    "$metric_response" --arg id "$metric_id" --arg name "$metric_name"

  updated_metric_name='Live event count updated [managed;o=acceptance;m=sko]'
  jq -n --arg name "$updated_metric_name" --arg project "$project" '{
    name: $name,
    aggregate: "count()",
    query: "",
    timeWindow: "300",
    resolveThreshold: 100,
    thresholdType: 0,
    triggers: [{label: "critical", alertThreshold: 250, actions: []}],
    projects: [$project],
    activations: []
  }' >"$metric_request"
  request PUT "/api/0/organizations/$organization/alert-rules/$metric_id/" \
    "$metric_request" "$metric_response"
  request GET "/api/0/organizations/$organization/alert-rules/$metric_id/" '' \
    "$metric_response"
  assert_json "legacy metric alert update round-trips without changing identity" \
    '(.id | tostring) == $id and .name == $name and .triggers[0].alertThreshold == 250' \
    "$metric_response" --arg id "$metric_id" --arg name "$updated_metric_name"

  request DELETE "/api/0/organizations/$organization/alert-rules/$metric_id/" '' \
    "$metric_response"
  metric_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 30 --output "$metric_response" --write-out '%{http_code}' \
    "$base_url/api/0/organizations/$organization/alert-rules/$metric_id/")
  if [ "$metric_status" != 404 ]; then
    echo "deleted legacy metric alert remained readable: HTTP $metric_status" >&2
    exit 1
  fi
else
  # Sentry 26 publishes only POST on the project-detectors endpoint; GET is
  # intentionally forbidden even with alerts:read. The default_rules=false
  # effect remains covered by the source/contract test for this version.
  workflow_request="$temporary_dir/workflow-request.json"
  workflow_response="$temporary_dir/workflow-response.json"
  workflow_name='Live workflow [managed;o=acceptance;m=sko]'
  jq -n --arg name "$workflow_name" '{
    name: $name,
    enabled: true,
    config: {},
    triggers: {logicType: "any", conditions: []},
    actionFilters: []
  }' >"$workflow_request"
  request POST "/api/0/organizations/$organization/workflows/" \
    "$workflow_request" "$workflow_response"
  assert_json "modern workflow is created with its ownership marker" \
    '.name == $name and .enabled == true and (.id | type == "string")' \
    "$workflow_response" --arg name "$workflow_name"
  workflow_id=$(jq -r '.id' "$workflow_response")
  if ! printf '%s\n' "$workflow_id" | grep -Eq '^[0-9]+$'; then
    echo 'modern workflow returned an invalid identity' >&2
    exit 1
  fi

  request GET "/api/0/organizations/$organization/workflows/$workflow_id/" '' \
    "$workflow_response"
  assert_json "modern workflow is readable by exact identity" \
    '.id == $id and .name == $name and .enabled == true' \
    "$workflow_response" --arg id "$workflow_id" --arg name "$workflow_name"

  updated_workflow_name='Live workflow updated [managed;o=acceptance;m=sko]'
  jq -n --arg name "$updated_workflow_name" '{
    name: $name,
    enabled: true,
    config: {},
    triggers: {logicType: "any", conditions: []},
    actionFilters: []
  }' >"$workflow_request"
  request PUT "/api/0/organizations/$organization/workflows/$workflow_id/" \
    "$workflow_request" "$workflow_response"
  assert_json "modern workflow update round-trips without changing identity" \
    '.id == $id and .name == $name and .enabled == true' \
    "$workflow_response" --arg id "$workflow_id" --arg name "$updated_workflow_name"

  jq -n '{enabled: false}' >"$workflow_request"
  request PUT "/api/0/organizations/$organization/workflows/?id=$workflow_id" \
    "$workflow_request" "$workflow_response"
  assert_json "modern workflow can be disabled by exact identity" \
    'type == "array" and length == 1 and .[0].id == $id and .[0].enabled == false' \
    "$workflow_response" --arg id "$workflow_id"
  jq -n '{enabled: true}' >"$workflow_request"
  request PUT "/api/0/organizations/$organization/workflows/?id=$workflow_id" \
    "$workflow_request" "$workflow_response"
  assert_json "modern workflow can be re-enabled by exact identity" \
    'type == "array" and length == 1 and .[0].id == $id and .[0].enabled == true' \
    "$workflow_response" --arg id "$workflow_id"

  request DELETE "/api/0/organizations/$organization/workflows/$workflow_id/" '' \
    "$workflow_response"
  workflow_attempt=0
  workflow_status=
  while [ "$workflow_attempt" -lt 30 ]; do
    workflow_attempt=$((workflow_attempt + 1))
    workflow_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 10 --output "$workflow_response" --write-out '%{http_code}' \
      "$base_url/api/0/organizations/$organization/workflows/$workflow_id/")
    case "$workflow_status" in
      404) break ;;
      200) sleep 1 ;;
      *) echo "modern workflow delete verification returned HTTP $workflow_status" >&2; exit 1 ;;
    esac
  done
  if [ "$workflow_status" != 404 ]; then
    echo 'modern workflow remained readable after delete' >&2
    exit 1
  fi
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

echo "Sentry $version live API project/settings/ingestion/alerts/delete acceptance passed"
