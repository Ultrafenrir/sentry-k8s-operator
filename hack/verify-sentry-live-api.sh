#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=${SENTRY_VERSION:-26.10.1}
cache_dir=${SENTRY_SELF_HOSTED_CACHE:-$root_dir/.cache/sentry-self-hosted}
config_only=${SENTRY_LIVE_CONFIG_ONLY:-false}

case "$config_only" in
  true|false) ;;
  *) echo 'SENTRY_LIVE_CONFIG_ONLY must be true or false' >&2; exit 1 ;;
esac

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
recovery_project="sko-lost-response"
recovery_name='Lost response [managed;o=acceptance;m=sko]'
filter_project="sko-filter-semantics"
filter_name='Filter semantics [managed;o=acceptance;m=sko]'
filter_project_created=false

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

# Client-key responses contain server-generated secret material. This assertion must never
# print a response body, even on failure.
assert_sensitive_json() {
  description=$1
  filter=$2
  file=$3
  shift 3
  if ! jq -e "$@" "$filter" "$file" >/dev/null; then
    echo "live API assertion failed for secret-bearing response: $description" >&2
    exit 1
  fi
}

remove_stale_fixture_project() {
  stale_slug=$1
  stale_output="$temporary_dir/stale-$stale_slug.json"
  stale_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 30 --output "$stale_output" --write-out '%{http_code}' \
    "$base_url/api/0/projects/$organization/$stale_slug/")
  case "$stale_status" in
    404) return 0 ;;
    200) ;;
    *) echo "stale fixture lookup for $stale_slug returned HTTP $stale_status" >&2; exit 1 ;;
  esac
  stale_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 30 --request DELETE --output "$stale_output" \
    --write-out '%{http_code}' "$base_url/api/0/projects/$organization/$stale_slug/")
  case "$stale_status" in
    2??|404) ;;
    *) echo "stale fixture delete for $stale_slug returned HTTP $stale_status" >&2; exit 1 ;;
  esac
  stale_attempt=0
  while [ "$stale_attempt" -lt 30 ]; do
    stale_attempt=$((stale_attempt + 1))
    stale_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 10 --output "$stale_output" --write-out '%{http_code}' \
      "$base_url/api/0/projects/$organization/$stale_slug/")
    case "$stale_status" in
      404) return 0 ;;
      200) sleep 1 ;;
      *) echo "stale fixture verification for $stale_slug returned HTTP $stale_status" >&2; exit 1 ;;
    esac
  done
  echo "stale fixture $stale_slug remained readable after delete" >&2
  exit 1
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

# A failed acceptance run may stop before its normal delete phase. These exact
# fixture slugs belong only to this disposable organization, so make reruns
# converge instead of requiring another full Sentry installation.
for stale_project in "$project" "$recovery_project" "$filter_project"; do
  remove_stale_fixture_project "$stale_project"
done

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

project_identity=$(jq -r '.id // empty' "$project_get")
source_integrations="$temporary_dir/source-integrations.json"
source_repositories="$temporary_dir/source-repositories.json"
code_mapping_request="$temporary_dir/code-mapping-request.json"
code_mapping_response="$temporary_dir/code-mapping-response.json"
codeowners_request="$temporary_dir/codeowners-request.json"
codeowners_response="$temporary_dir/codeowners-response.json"
request GET "/api/0/organizations/$organization/integrations/?provider_key=github" '' "$source_integrations"
assert_json "disposable source-code integration fixture is listed exactly once" \
  '([.[] | select(.name == "SKO local source fixture" and .provider.key == "github" and (.id | type == "string"))] | length) == 1' \
  "$source_integrations"
source_integration_id=$(jq -r '.[] | select(.name == "SKO local source fixture" and .provider.key == "github") | .id' "$source_integrations")
request GET "/api/0/organizations/$organization/repos/?integration_id=$source_integration_id" '' "$source_repositories"
assert_json "disposable repository fixture is scoped to the exact integration" \
  '([.[] | select(.name == "sko/local-repository" and (.id | type == "string") and ((.integrationId | tostring) == $integration))] | length) == 1' \
  "$source_repositories" --arg integration "$source_integration_id"
source_repository_id=$(jq -r '.[] | select(.name == "sko/local-repository") | .id' "$source_repositories")
jq -n --arg integration "$source_integration_id" --arg repository "$source_repository_id" --arg project "$project_identity" \
  '{integrationId:$integration, repositoryId:$repository, projectId:$project, stackRoot:"/", sourceRoot:"src", defaultBranch:"main"}' >"$code_mapping_request"
request POST "/api/0/organizations/$organization/code-mappings/" "$code_mapping_request" "$code_mapping_response"
assert_json "repository code mapping creates with exact integration, repository, project, and paths" \
  '(.id | tostring | test("^[0-9]+$")) and ((.integrationId | tostring) == $integration) and ((.repoId | tostring) == $repository) and ((.projectId | tostring) == $project) and .stackRoot == "/" and .sourceRoot == "src" and .defaultBranch == "main"' \
  "$code_mapping_response" --arg integration "$source_integration_id" --arg repository "$source_repository_id" --arg project "$project_identity"
code_mapping_id=$(jq -r '.id | tostring' "$code_mapping_response")
jq -n --arg integration "$source_integration_id" --arg repository "$source_repository_id" --arg project "$project_identity" \
  '{integrationId:$integration, repositoryId:$repository, projectId:$project, stackRoot:"/srv/app", sourceRoot:"src", defaultBranch:"main"}' >"$code_mapping_request"
request PUT "/api/0/organizations/$organization/code-mappings/$code_mapping_id/" "$code_mapping_request" "$code_mapping_response"
assert_json "repository code mapping update preserves identity and round-trips drift-repair fields" \
  '(.id | tostring) == $id and .stackRoot == "/srv/app" and .sourceRoot == "src" and .defaultBranch == "main"' \
  "$code_mapping_response" --arg id "$code_mapping_id"

codeowners_raw='src/* #sko-live'
jq -n --arg mapping "$code_mapping_id" --arg raw "$codeowners_raw" '{codeMappingId:$mapping, raw:$raw}' >"$codeowners_request"
request POST "/api/0/projects/$organization/$project/codeowners/" "$codeowners_request" "$codeowners_response"
assert_json "CODEOWNERS child stores raw content against the exact code mapping" \
  '(.id | tostring | test("^[0-9]+$")) and ((.codeMappingId | tostring) == $mapping) and .raw == $raw' \
  "$codeowners_response" --arg mapping "$code_mapping_id" --arg raw "$codeowners_raw"
codeowners_id=$(jq -r '.id | tostring' "$codeowners_response")

recovery_request="$temporary_dir/recovery-request.json"
recovery_response="$temporary_dir/recovery-response.json"
jq -n --arg name "$recovery_name" --arg slug "$recovery_project" \
  '{name: $name, slug: $slug, default_rules: false}' >"$recovery_request"
# Deliberately discard the successful create response. Recovery must use a safe GET
# keyed by the intended slug and must verify the complete ownership marker.
recovery_create_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
  --connect-timeout 2 --max-time 30 --request POST \
  --header 'Content-Type: application/json' --data-binary "@$recovery_request" \
  --output /dev/null --write-out '%{http_code}' \
  "$base_url/api/0/teams/$organization/$team/projects/")
case "$recovery_create_status" in
  2??) ;;
  *) echo "discarded project create returned HTTP $recovery_create_status" >&2; exit 1 ;;
esac
request GET "/api/0/projects/$organization/$recovery_project/" '' "$recovery_response"
assert_json "lost project-create response is recovered by slug and exact marker" \
  '.slug == $slug and .name == $name and (.id | type == "string")' \
  "$recovery_response" --arg slug "$recovery_project" --arg name "$recovery_name"

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

managed_key_request="$temporary_dir/managed-key-request.json"
managed_key_response="$temporary_dir/managed-key-response.json"
managed_key_detail="$temporary_dir/managed-key-detail.json"
managed_key_name="sko:api:$(printf '%s' "$version" | tr -d '.'):r1"
jq -n --arg name "$managed_key_name" '{name: $name, rateLimit: {count: 1000, window: 60}}' >"$managed_key_request"
request POST "/api/0/projects/$organization/$project/keys/" "$managed_key_request" "$managed_key_response"
assert_sensitive_json "managed client key create" '
  .name == $name and .isActive == true and
  .rateLimit.count == 1000 and .rateLimit.window == 60 and
  (.id | type == "string") and (.dsn.public | type == "string")
' "$managed_key_response" --arg name "$managed_key_name"
managed_key_id=$(jq -r '.id // empty' "$managed_key_response")
if ! printf '%s\n' "$managed_key_id" | grep -Eq '^[0-9a-f]{32}$'; then
  echo 'created managed client key has an invalid identity' >&2
  exit 1
fi

jq -n --arg name "$managed_key_name" '{name: $name, isActive: false, rateLimit: {count: 2000, window: 120}}' >"$managed_key_request"
request PUT "/api/0/projects/$organization/$project/keys/$managed_key_id/" "$managed_key_request" "$managed_key_response"
request GET "/api/0/projects/$organization/$project/keys/$managed_key_id/" '' "$managed_key_detail"
assert_sensitive_json "managed client key update read-back" '
  .id == $identity and .name == $name and .isActive == false and
  .rateLimit.count == 2000 and .rateLimit.window == 120
' "$managed_key_detail" --arg identity "$managed_key_id" --arg name "$managed_key_name"

replacement_key_name="sko:api:$(printf '%s' "$version" | tr -d '.'):r2"
jq -n --arg name "$replacement_key_name" '{name: $name}' >"$managed_key_request"
request POST "/api/0/projects/$organization/$project/keys/" "$managed_key_request" "$managed_key_response"
assert_sensitive_json "managed client key replacement create" '
  .name == $name and .isActive == true and (.id | type == "string") and
  (.dsn.public | type == "string")
' "$managed_key_response" --arg name "$replacement_key_name"
replacement_key_id=$(jq -r '.id // empty' "$managed_key_response")
if ! printf '%s\n' "$replacement_key_id" | grep -Eq '^[0-9a-f]{32}$' || [ "$replacement_key_id" = "$managed_key_id" ]; then
  echo 'replacement managed client key has an invalid identity' >&2
  exit 1
fi

request DELETE "/api/0/projects/$organization/$project/keys/$managed_key_id/" '' "$managed_key_response"
managed_key_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
  --connect-timeout 2 --max-time 30 --output "$managed_key_detail" --write-out '%{http_code}' \
  "$base_url/api/0/projects/$organization/$project/keys/$managed_key_id/")
if [ "$managed_key_status" != 404 ]; then
  echo "managed client key remained readable after delete: HTTP $managed_key_status" >&2
  exit 1
fi
request DELETE "/api/0/projects/$organization/$project/keys/$replacement_key_id/" '' "$managed_key_response"
replacement_key_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
  --connect-timeout 2 --max-time 30 --output "$managed_key_detail" --write-out '%{http_code}' \
  "$base_url/api/0/projects/$organization/$project/keys/$replacement_key_id/")
if [ "$replacement_key_status" != 404 ]; then
  echo "replacement managed client key remained readable after delete: HTTP $replacement_key_status" >&2
  exit 1
fi

filters_response="$temporary_dir/filters-response.json"
request GET "/api/0/projects/$organization/$project/filters/" '' "$filters_response"
assert_json "published inbound filters are listed" '
  type == "array" and
  ([.[] | select(.id == "browser-extensions" and (.active | type == "boolean"))] | length) == 1 and
  ([.[] | select(.id == "localhost" and (.active | type == "boolean"))] | length) == 1 and
  ([.[] | select(.id == "web-crawlers" and (.active | type == "boolean"))] | length) == 1 and
  ([.[] | select(.id == "filtered-transaction" and (.active | type == "boolean"))] | length) == 1 and
  ([.[] | select(.id == "legacy-browsers" and ((.active | type) == "boolean" or (.active | type) == "array"))] | length) == 1
' "$filters_response"

filter_request="$temporary_dir/filter-request.json"
filter_response="$temporary_dir/filter-response.json"
for filter_state in 'browser-extensions false' 'localhost true' 'web-crawlers false' 'filtered-transaction true'; do
  filter_id=${filter_state% *}
  filter_active=${filter_state#* }
  jq -n --argjson active "$filter_active" '{active: $active}' >"$filter_request"
  request PUT "/api/0/projects/$organization/$project/filters/$filter_id/" \
    "$filter_request" "$filter_response"
done
jq -n '{subfilters: ["ie", "chrome"]}' >"$filter_request"
request PUT "/api/0/projects/$organization/$project/filters/legacy-browsers/" \
  "$filter_request" "$filter_response"
request GET "/api/0/projects/$organization/$project/filters/" '' "$filters_response"
assert_json "inbound filter boolean updates round-trip" '
  ([.[] | select(.id == "browser-extensions" and .active == false)] | length) == 1 and
  ([.[] | select(.id == "localhost" and .active == true)] | length) == 1 and
  ([.[] | select(.id == "web-crawlers" and .active == false)] | length) == 1 and
  ([.[] | select(.id == "filtered-transaction" and .active == true)] | length) == 1 and
  ([.[] | select(.id == "legacy-browsers" and (.active | sort) == ["chrome", "ie"])] | length) == 1
' "$filters_response"

settings_request="$temporary_dir/settings-request.json"
settings_response="$temporary_dir/settings-response.json"
relay_pii_config='{"rules":{"0":{"type":"email","redaction":{"method":"remove"}},"1":{"type":"anything","redaction":{"method":"replace","text":"[Filtered]"}}},"applications":{"$message":["0"],"extra.customer.email":["0"],"request.headers.Authorization":["1"]}}'
grouping_enhancements='stack.abs_path:"*/node_modules/*" -app
!stack.function:"vendor_*" +group'
fingerprinting_rules='error.type:"DatabaseUnavailable" -> "database-unavailable"
stack.function:"query_*" -> "database", {{ default }}'
jq -n --arg relay_pii_config "$relay_pii_config" --arg grouping_enhancements "$grouping_enhancements" --arg fingerprinting_rules "$fingerprinting_rules" '{
  dataScrubber: true,
  dataScrubberDefaults: true,
  scrubIPAddresses: true,
  sensitiveFields: ["authorization", "password"],
  safeFields: ["request.headers.x-request-id"],
  resolveAge: 72,
  defaultEnvironment: "production",
  storeCrashReports: 10,
  scrapeJavaScript: false,
  allowedDomains: ["*.checkout.example.com", "https://checkout.example.com:443"],
  relayPiiConfig: $relay_pii_config,
  groupingEnhancements: $grouping_enhancements,
  fingerprintingRules: $fingerprinting_rules,
  builtinSymbolSources: ["electron", "microsoft", "nuget"]
}' >"$settings_request"
request PUT "/api/0/projects/$organization/$project/" "$settings_request" "$settings_response"
request GET "/api/0/projects/$organization/$project/" '' "$settings_response"
assert_json "typed PII settings round-trip" '
  .dataScrubber == true and
  .dataScrubberDefaults == true and
  .scrubIPAddresses == true and
  .sensitiveFields == ["authorization", "password"] and
  .safeFields == ["request.headers.x-request-id"] and
  .resolveAge == 72 and
  .defaultEnvironment == "production" and
  .storeCrashReports == 10 and
  .scrapeJavaScript == false and
  (.allowedDomains | sort) == ["*.checkout.example.com", "https://checkout.example.com:443"] and
  (.relayPiiConfig | fromjson | (.rules["0"].type == "email") and (.applications["$message"] == ["0"])) and
  .groupingEnhancements == $grouping_enhancements and
  .fingerprintingRules == $fingerprinting_rules and
  (.builtinSymbolSources | sort) == ["electron", "microsoft", "nuget"]
' "$settings_response" --arg grouping_enhancements "$grouping_enhancements" --arg fingerprinting_rules "$fingerprinting_rules"

symbol_source_request="$temporary_dir/symbol-source-request.json"
symbol_source_response="$temporary_dir/symbol-source-response.json"
jq -n '{
  id: "sko-live-http-source",
  name: "sko:http-symbols:live",
  type: "http",
  url: "https://symbols.example.invalid",
  username: "reader",
  password: "sko-live-symbol-secret-one",
  layout: {type: "native", casing: "default"}
}' >"$symbol_source_request"
request POST "/api/0/projects/$organization/$project/symbol-sources/" "$symbol_source_request" "$symbol_source_response"
assert_sensitive_json "HTTP symbol source create redacts its password" '
  .id == "sko-live-http-source" and
  .name == "sko:http-symbols:live" and
  .type == "http" and
  .url == "https://symbols.example.invalid" and
  .password == {"hidden-secret": true}
' "$symbol_source_response"

jq -n '{
  id: "sko-live-s3-source",
  name: "sko:s3-symbols:live",
  type: "s3",
  bucket: "sko-live-symbols",
  region: "eu-west-1",
  access_key: "sko-live-access-key",
  secret_key: "sko-live-secret-key",
  prefix: "native/",
  layout: {type: "symstore", casing: "lowercase"}
}' >"$symbol_source_request"
request POST "/api/0/projects/$organization/$project/symbol-sources/" "$symbol_source_request" "$symbol_source_response"
assert_sensitive_json "S3 symbol source create redacts its secret key" '
  .id == "sko-live-s3-source" and .type == "s3" and
  .secret_key == {"hidden-secret": true}
' "$symbol_source_response"

jq -n '{
  id: "sko-live-gcs-source",
  name: "sko:gcs-symbols:live",
  type: "gcs",
  bucket: "sko-live-symbols",
  client_email: "symbols@example.invalid",
  private_key: "sko-live-private-key",
  prefix: "debug/",
  layout: {type: "unified", casing: "uppercase"}
}' >"$symbol_source_request"
request POST "/api/0/projects/$organization/$project/symbol-sources/" "$symbol_source_request" "$symbol_source_response"
assert_sensitive_json "GCS symbol source create redacts its private key" '
  .id == "sko-live-gcs-source" and .type == "gcs" and
  .private_key == {"hidden-secret": true}
' "$symbol_source_response"

request GET "/api/0/projects/$organization/$project/symbol-sources/" '' "$symbol_source_response"
assert_sensitive_json "custom symbol source list contains each exact owned identity" '
  ([.[] | select(.id == "sko-live-http-source" and .password == {"hidden-secret": true})] | length) == 1 and
  ([.[] | select(.id == "sko-live-s3-source" and .secret_key == {"hidden-secret": true})] | length) == 1 and
  ([.[] | select(.id == "sko-live-gcs-source" and .private_key == {"hidden-secret": true})] | length) == 1
' "$symbol_source_response"

jq -n '{
  id: "sko-live-http-source",
  name: "sko:http-symbols:live",
  type: "http",
  url: "https://symbols-updated.example.invalid",
  username: "reader",
  password: "sko-live-symbol-secret-two",
  layout: {type: "native", casing: "default"}
}' >"$symbol_source_request"
request PUT "/api/0/projects/$organization/$project/symbol-sources/?id=sko-live-http-source" "$symbol_source_request" "$symbol_source_response"
assert_sensitive_json "HTTP symbol source update acknowledges non-secret drift and redacts rotated password" '
  .id == "sko-live-http-source" and
  .url == "https://symbols-updated.example.invalid" and
  .password == {"hidden-secret": true}
' "$symbol_source_response"

for source_id in sko-live-http-source sko-live-s3-source sko-live-gcs-source; do
  request DELETE "/api/0/projects/$organization/$project/symbol-sources/?id=$source_id" '' "$symbol_source_response"
done
request GET "/api/0/projects/$organization/$project/symbol-sources/" '' "$symbol_source_response"
assert_sensitive_json "exact custom symbol source cleanup removes all three owned identities" '
  ([.[] | select(.id | startswith("sko-live-"))] | length) == 0
' "$symbol_source_response"

ownership_request="$temporary_dir/ownership-request.json"
ownership_response="$temporary_dir/ownership-response.json"
ownership_raw=$(printf 'path:src/checkout/* #%s\ntags.environment:production #%s' "$team" "$team")
jq -n --arg raw "$ownership_raw" '{
  raw: $raw,
  fallthrough: false,
  autoAssignment: "Auto Assign to Issue Owner",
  codeownersAutoSync: false
}' >"$ownership_request"
request PUT "/api/0/projects/$organization/$project/ownership/" "$ownership_request" "$ownership_response"
request GET "/api/0/projects/$organization/$project/ownership/" '' "$ownership_response"
assert_json "team-only issue ownership configuration round-trip" '
  .raw == $raw and
  .fallthrough == false and
  .autoAssignment == "Auto Assign to Issue Owner" and
  .codeownersAutoSync == false and
  ([.schema.rules[].owners[] | select(.type == "team" and .name == $team)] | length) == 2 and
  ([.schema.rules[].owners[] | select(.type == "user")] | length) == 0
' "$ownership_response" --arg raw "$ownership_raw" --arg team "$team"

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

if [ "$config_only" = false ]; then
event_request="$temporary_dir/event-request.json"
event_response="$temporary_dir/event-response.json"
ingest_event() {
  description=$1
  input=$2
  ingest_status=$(curl --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 30 --request POST \
    --header 'Content-Type: application/json' --data-binary "@$input" \
    --output "$event_response" --write-out '%{http_code}' \
    "$base_url/api/$project_id/store/?sentry_key=$public_key&sentry_version=7")
  case "$ingest_status" in
    2??) ;;
    *) echo "$description ingestion returned HTTP $ingest_status" >&2; exit 1 ;;
  esac
}

wait_for_event() {
  expected_event_id=$1
  description=$2
  event_attempt=0
  event_status=
  while [ "$event_attempt" -lt 120 ]; do
    event_attempt=$((event_attempt + 1))
    event_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 10 --output "$event_response" --write-out '%{http_code}' \
      "$base_url/api/0/projects/$organization/$project/events/$expected_event_id/")
    case "$event_status" in
      200) break ;;
      404) sleep 1 ;;
      *) echo "$description lookup returned HTTP $event_status" >&2; exit 1 ;;
    esac
  done
  if [ "$event_status" != 200 ]; then
    echo "$description was not readable after ingestion" >&2
    exit 1
  fi
}

assert_event_filtered() {
  expected_event_id=$1
  description=$2
  filtered_attempt=0
  while [ "$filtered_attempt" -lt 20 ]; do
    filtered_attempt=$((filtered_attempt + 1))
    filtered_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 10 --output "$event_response" --write-out '%{http_code}' \
      "$base_url/api/0/projects/$organization/$project/events/$expected_event_id/")
    case "$filtered_status" in
      404) sleep 1 ;;
      200) echo "$description became readable" >&2; exit 1 ;;
      *) echo "$description lookup returned HTTP $filtered_status" >&2; exit 1 ;;
    esac
  done
}

case "$version" in
  24.7.1) event_id=24710000000000000000000000000001 ;;
  26.10.1) event_id=26101000000000000000000000000001 ;;
esac
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
ingest_event 'synthetic PII event' "$event_request"
wait_for_event "$event_id" 'synthetic PII event'
assert_json "ingested event is returned by exact identity with sensitive extras scrubbed" '
  .eventID == $event_id and
  .message == "Synthetic PII acceptance" and
  .context.authorization == "[Filtered]" and
  .context.password == "[Filtered]" and
  .context.request_id == "sko-safe-request-id" and
  (tostring | contains("sko-sensitive-authorization") | not) and
  (tostring | contains("sko-sensitive-password") | not)
' "$event_response" --arg event_id "$event_id"

case "$version" in
  24.7.1) filtered_event_id=24710000000000000000000000000002 ;;
  26.10.1) filtered_event_id=26101000000000000000000000000002 ;;
esac
jq -n --arg event_id "$filtered_event_id" '{
  event_id: $event_id,
  platform: "other",
  logger: "sko-live",
  level: "error",
  message: "Synthetic localhost filter acceptance",
  environment: "sko-live",
  user: {ip_address: "127.0.0.1"}
}' >"$event_request"
ingest_event 'localhost-filtered synthetic event' "$event_request"
assert_event_filtered "$filtered_event_id" 'localhost-filtered synthetic event'

case "$version" in
  24.7.1)
    extension_control_id=24710000000000000000000000000003
    extension_filtered_id=24710000000000000000000000000004
    crawler_control_id=24710000000000000000000000000005
    crawler_filtered_id=24710000000000000000000000000006
    ;;
  26.10.1)
    extension_control_id=26101000000000000000000000000003
    extension_filtered_id=26101000000000000000000000000004
    crawler_control_id=26101000000000000000000000000005
    crawler_filtered_id=26101000000000000000000000000006
    ;;
esac

jq -n --arg event_id "$extension_control_id" '{
  event_id: $event_id,
  platform: "javascript",
  logger: "sko-live",
  level: "error",
  message: "Synthetic browser extension control",
  exception: {values: [{type: "Error", value: "http://loading.retry.widdit.com/"}]}
}' >"$event_request"
ingest_event 'browser-extension disabled control event' "$event_request"
wait_for_event "$extension_control_id" 'browser-extension disabled control event'

jq -n --arg event_id "$crawler_control_id" '{
  event_id: $event_id,
  platform: "other",
  logger: "sko-live",
  level: "error",
  message: "Synthetic web crawler control",
  request: {
    url: "http://example.com",
    method: "GET",
    headers: [["User-Agent", "Mediapartners-Google"]]
  }
}' >"$event_request"
ingest_event 'web-crawler disabled control event' "$event_request"
wait_for_event "$crawler_control_id" 'web-crawler disabled control event'

filter_project_request="$temporary_dir/filter-project-request.json"
filter_project_response="$temporary_dir/filter-project-response.json"
jq -n --arg name "$filter_name" --arg slug "$filter_project" \
  '{name: $name, slug: $slug, default_rules: false}' >"$filter_project_request"
request POST "/api/0/teams/$organization/$team/projects/" \
  "$filter_project_request" "$filter_project_response"
filter_project_created=true
assert_json "filter-semantics project preserves its ownership marker" \
  '.slug == $slug and .name == $name and (.id | type == "string")' \
  "$filter_project_response" --arg slug "$filter_project" --arg name "$filter_name"

for filter_id in browser-extensions web-crawlers; do
  jq -n '{active: true}' >"$filter_request"
  request PUT "/api/0/projects/$organization/$filter_project/filters/$filter_id/" \
    "$filter_request" "$filter_response"
done
request GET "/api/0/projects/$organization/$filter_project/filters/" '' "$filters_response"
assert_json "enabled semantic filters round-trip before first ingestion" '
  ([.[] | select(.id == "browser-extensions" and .active == true)] | length) == 1 and
  ([.[] | select(.id == "web-crawlers" and .active == true)] | length) == 1
' "$filters_response"

filter_keys_response="$temporary_dir/filter-keys-response.json"
request GET "/api/0/projects/$organization/$filter_project/keys/" '' "$filter_keys_response"
filter_public_key=$(jq -r '[.[] | select(.isActive == true)][0].public // empty' "$filter_keys_response")
filter_project_id=$(jq -r '[.[] | select(.isActive == true)][0].projectId // empty' "$filter_keys_response")
if ! printf '%s\n' "$filter_public_key" | grep -Eq '^[0-9a-f]{32}$'; then
  echo 'filter-semantics project key has an invalid public key' >&2
  exit 1
fi
if ! printf '%s\n' "$filter_project_id" | grep -Eq '^[0-9]+$'; then
  echo 'filter-semantics project key has an invalid project identity' >&2
  exit 1
fi

primary_project=$project
primary_project_id=$project_id
primary_public_key=$public_key
project=$filter_project
project_id=$filter_project_id
public_key=$filter_public_key

jq -n --arg event_id "$extension_filtered_id" '{
  event_id: $event_id,
  platform: "javascript",
  logger: "sko-live",
  level: "error",
  message: "Synthetic browser extension filtered",
  exception: {values: [{type: "Error", value: "http://loading.retry.widdit.com/"}]}
}' >"$event_request"
ingest_event 'browser-extension enabled filtered event' "$event_request"
assert_event_filtered "$extension_filtered_id" 'browser-extension enabled filtered event'

jq -n --arg event_id "$crawler_filtered_id" '{
  event_id: $event_id,
  platform: "other",
  logger: "sko-live",
  level: "error",
  message: "Synthetic web crawler filtered",
  request: {
    url: "http://example.com",
    method: "GET",
    headers: [["User-Agent", "Mediapartners-Google"]]
  }
}' >"$event_request"
ingest_event 'web-crawler enabled filtered event' "$event_request"
assert_event_filtered "$crawler_filtered_id" 'web-crawler enabled filtered event'

project=$primary_project
project_id=$primary_project_id
public_key=$primary_public_key
fi

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
  metric_name='Live user count [managed;o=acceptance;m=sko]'
  jq -n --arg name "$metric_name" --arg project "$project" '{
    name: $name,
    aggregate: "count_unique(user)",
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
    '(.id | tostring) == $id and .name == $name and .aggregate == "count_unique(user)"' \
    "$metric_response" --arg id "$metric_id" --arg name "$metric_name"

  updated_metric_name='Live user count updated [managed;o=acceptance;m=sko]'
  jq -n --arg name "$updated_metric_name" --arg project "$project" '{
    name: $name,
    aggregate: "count_unique(user)",
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

  detector_request="$temporary_dir/detector-request.json"
  detector_response="$temporary_dir/detector-response.json"
  detector_name='Live user count detector [managed;o=acceptance;m=sko]'
  jq -n --arg name "$detector_name" --argjson workflow_id "$workflow_id" '{
    name: $name,
    type: "metric_issue",
    enabled: true,
    dataSources: [{
      queryType: 0,
      dataset: "events",
      query: "",
      aggregate: "count_unique(user)",
      timeWindow: 300,
      environment: null,
      eventTypes: ["error"]
    }],
    conditionGroup: {
      logicType: "any",
      conditions: [
        {type: "gt", comparison: 200, conditionResult: 75},
        {type: "lte", comparison: 100, conditionResult: 0}
      ]
    },
    config: {thresholdPeriod: 1, detectionType: "static"},
    workflowIds: [$workflow_id]
  }' >"$detector_request"
  request POST "/api/0/organizations/$organization/projects/$project/detectors/" \
    "$detector_request" "$detector_response"
  assert_json "modern metric detector is created and attached to the workflow" '
    .name == $name and .type == "metric_issue" and .enabled == true and
    .dataSources[0].queryObj.snubaQuery.aggregate == "count_unique(tags[sentry:user])" and
    (.id | type == "string") and .projectId == $project_id
  ' "$detector_response" --arg name "$detector_name" --arg project_id "$project_id"
  detector_id=$(jq -r '.id' "$detector_response")
  if ! printf '%s\n' "$detector_id" | grep -Eq '^[0-9]+$'; then
    echo 'modern detector returned an invalid identity' >&2
    exit 1
  fi

  request GET "/api/0/organizations/$organization/detectors/$detector_id/" '' \
    "$detector_response"
  assert_json "modern metric detector is readable by exact identity" \
    '.id == $id and .name == $name and .enabled == true' \
    "$detector_response" --arg id "$detector_id" --arg name "$detector_name"

  jq -n '{enabled: false}' >"$detector_request"
  request PUT "/api/0/organizations/$organization/detectors/?id=$detector_id" \
    "$detector_request" "$detector_response"
  assert_json "modern metric detector can be disabled by exact identity" \
    'type == "array" and length == 1 and .[0].id == $id and .[0].enabled == false' \
    "$detector_response" --arg id "$detector_id"
  jq -n '{enabled: true}' >"$detector_request"
  request PUT "/api/0/organizations/$organization/detectors/?id=$detector_id" \
    "$detector_request" "$detector_response"
  assert_json "modern metric detector can be re-enabled by exact identity" \
    'type == "array" and length == 1 and .[0].id == $id and .[0].enabled == true' \
    "$detector_response" --arg id "$detector_id"

  request DELETE "/api/0/organizations/$organization/detectors/$detector_id/" '' \
    "$detector_response"
  detector_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 10 --output "$detector_response" --write-out '%{http_code}' \
    "$base_url/api/0/organizations/$organization/detectors/$detector_id/")
  if [ "$detector_status" != 404 ]; then
    echo "modern detector remained readable after delete: HTTP $detector_status" >&2
    exit 1
  fi

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

service_hook_endpoint='https://hooks.example.invalid/events/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
service_hook_staging_endpoint='https://hooks.example.invalid/events/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
service_hook_request="$temporary_dir/service-hook-request.json"
service_hook_response="$temporary_dir/service-hook-response.json"
jq -n --arg url "$service_hook_staging_endpoint" \
  '{url: $url, events: ["event.alert"], version: 0, isActive: false}' >"$service_hook_request"
request POST "/api/0/projects/$organization/$project/hooks/" \
  "$service_hook_request" "$service_hook_response"
assert_sensitive_json "service hook create exposes the pinned active-on-create behavior and a generated secret" '
  (.id | type == "string" and length > 0) and
  .url == $url and .events == ["event.alert"] and .status == "active" and
  (.secret | type == "string" and length >= 16 and length <= 512)
' "$service_hook_response" --arg url "$service_hook_staging_endpoint"
service_hook_id=$(jq -r '.id' "$service_hook_response")
request GET "/api/0/projects/$organization/$project/hooks/$service_hook_id/" '' \
  "$service_hook_response"
assert_sensitive_json "staged service hook is independently readable by exact identity" '
  .id == $id and .url == $url and .events == ["event.alert"] and .status == "active" and
  (.secret | type == "string" and length >= 16 and length <= 512)
' "$service_hook_response" --arg id "$service_hook_id" --arg url "$service_hook_staging_endpoint"
jq -n --arg url "$service_hook_endpoint" \
  '{url: $url, events: ["event.alert"], version: 0, isActive: true}' >"$service_hook_request"
request PUT "/api/0/projects/$organization/$project/hooks/$service_hook_id/" \
  "$service_hook_request" "$service_hook_response"
assert_sensitive_json "service hook activation is readable from the mutation response" '
  .id == $id and .url == $url and .events == ["event.alert"] and .status == "active" and
  (.secret | type == "string" and length >= 16 and length <= 512)
' "$service_hook_response" --arg id "$service_hook_id" --arg url "$service_hook_endpoint"
request DELETE "/api/0/projects/$organization/$project/hooks/$service_hook_id/" '' \
  "$service_hook_response"
service_hook_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
  --connect-timeout 2 --max-time 10 --output "$service_hook_response" --write-out '%{http_code}' \
  "$base_url/api/0/projects/$organization/$project/hooks/$service_hook_id/")
if [ "$service_hook_status" != 404 ]; then
  echo "service hook remained readable after delete: HTTP $service_hook_status" >&2
  exit 1
fi

api_report="$temporary_dir/api-report.json"
SENTRY_VERSION="$version" SENTRY_PORT="$port" SENTRY_SELF_HOSTED_CACHE="$cache_dir" \
  sh "$root_dir/hack/report-sentry-live-api.sh" >"$api_report"
assert_json "read-only redacted API report covers the live project" '
  .version == $version and
  .organization == $organization and
  ([.projects[] | select(.slug == $project)] | length) == 1 and
  (.projects[] | select(.slug == $project) | .keys | length) >= 1 and
  (.alerts.adapter == $adapter)
' "$api_report" --arg version "$version" --arg organization "$organization" \
  --arg project "$project" --arg adapter "$(if [ "$version" = 24.7.1 ]; then printf '%s' legacy-24.7.1; else printf '%s' modern-26.10.1; fi)"

delete_response="$temporary_dir/delete-response.json"
request DELETE "/api/0/projects/$organization/$project/codeowners/$codeowners_id/" '' "$codeowners_response"
request DELETE "/api/0/organizations/$organization/code-mappings/$code_mapping_id/" '' "$code_mapping_response"
request GET "/api/0/organizations/$organization/code-mappings/" '' "$code_mapping_response"
assert_json "exact repository code mapping cleanup converges" \
  '([.[] | select((.id | tostring) == $id)] | length) == 0' "$code_mapping_response" --arg id "$code_mapping_id"
request DELETE "/api/0/projects/$organization/$recovery_project/" '' "$delete_response"
if [ "$filter_project_created" = true ]; then
  request DELETE "/api/0/projects/$organization/$filter_project/" '' "$delete_response"
fi
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

recovery_delete_attempt=0
recovery_status=
while [ "$recovery_delete_attempt" -lt 30 ]; do
  recovery_delete_attempt=$((recovery_delete_attempt + 1))
  recovery_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 10 --output "$recovery_response" --write-out '%{http_code}' \
    "$base_url/api/0/projects/$organization/$recovery_project/")
  case "$recovery_status" in
    404) break ;;
    200) sleep 1 ;;
    *) echo "recovered project delete verification returned HTTP $recovery_status" >&2; exit 1 ;;
  esac
done
if [ "$recovery_status" != 404 ]; then
  echo "recovered project remained readable after delete: HTTP $recovery_status" >&2
  exit 1
fi

if [ "$filter_project_created" = true ]; then
  filter_delete_attempt=0
  filter_delete_status=
  while [ "$filter_delete_attempt" -lt 30 ]; do
    filter_delete_attempt=$((filter_delete_attempt + 1))
    filter_delete_status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
      --connect-timeout 2 --max-time 10 --output "$filter_project_response" --write-out '%{http_code}' \
      "$base_url/api/0/projects/$organization/$filter_project/")
    case "$filter_delete_status" in
      404) break ;;
      200) sleep 1 ;;
      *) echo "filter-semantics project delete verification returned HTTP $filter_delete_status" >&2; exit 1 ;;
    esac
  done
  if [ "$filter_delete_status" != 404 ]; then
    echo "filter-semantics project remained readable after delete: HTTP $filter_delete_status" >&2
    exit 1
  fi
fi

evidence_dir="$cache_dir/evidence"
evidence_file="$evidence_dir/$version-capabilities.json"
mkdir -p "$evidence_dir"
chmod 700 "$evidence_dir"
evidence_temporary="$temporary_dir/capability-evidence.json"
jq -n --arg version "$version" --arg organization "$organization" --arg gate "$(if [ "$config_only" = true ]; then printf '%s' sentry-live-config-api; else printf '%s' sentry-live-api; fi)" '
  {
    version: $version,
    organization: $organization,
    gate: $gate,
    capabilities: [
      "project.create.atomic-default-rules",
      "project.create.automatic-active-key"
    ]
  }
' >"$evidence_temporary"
chmod 600 "$evidence_temporary"
mv -f -- "$evidence_temporary" "$evidence_file"

if [ "$config_only" = true ]; then
  echo "Sentry $version live configuration API recovery/client-key/settings/symbol-sources/code-mappings/CODEOWNERS-storage/issue-ownership/service-hooks/report/alerts-including-user-count/delete acceptance passed (event ingestion intentionally skipped)"
else
  echo "Sentry $version live API recovery/client-key/settings/symbol-sources/code-mappings/CODEOWNERS-storage/issue-ownership/service-hooks/report/filtering/alerts-including-user-count/delete acceptance passed"
fi
