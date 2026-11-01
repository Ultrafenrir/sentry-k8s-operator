#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=${SENTRY_VERSION:-26.10.1}
cache_dir=${SENTRY_SELF_HOSTED_CACHE:-$root_dir/.cache/sentry-self-hosted}

case "$version" in
  24.7.1) port=${SENTRY_PORT:-24710}; version_slug=24-7-1 ;;
  26.10.1) port=${SENTRY_PORT:-26101}; version_slug=26-10-1 ;;
  *) echo "unsupported Sentry version: $version" >&2; exit 1 ;;
esac
case "$cache_dir" in
  /*) ;;
  *) echo 'SENTRY_SELF_HOSTED_CACHE must be an absolute path' >&2; exit 1 ;;
esac
for command in curl jq stat; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command is missing: $command" >&2; exit 1; }
done

token_file="$cache_dir/tokens/$version.token"
if [ ! -f "$token_file" ] || [ "$(stat -f '%Lp' "$token_file")" != 600 ]; then
  echo "run make sentry-bootstrap SENTRY_VERSION=$version first" >&2
  exit 1
fi
report_dir=$(mktemp -d /tmp/sko-sentry-api-report.XXXXXX)
cleanup() {
  unset report_token
  case "$report_dir" in
    /tmp/sko-sentry-api-report.*) rm -rf -- "$report_dir" ;;
    *) echo "refusing unexpected report temporary path: $report_dir" >&2 ;;
  esac
}
trap cleanup EXIT INT TERM

report_token=$(sed -n '1p' "$token_file")
if ! printf '%s\n' "$report_token" | grep -Eq '^sntryu_[0-9a-f]{64}$'; then
  echo 'managed local Sentry token has an invalid format' >&2
  exit 1
fi
auth_config="$report_dir/curl-auth.conf"
(umask 077; printf 'header = "Authorization: Bearer %s"\n' "$report_token" >"$auth_config")
unset report_token

base_url="http://127.0.0.1:$port"
organization="sko-live-$version_slug"
request() {
  path=$1
  output=$2
  status=$(curl --config "$auth_config" --silent --show-error --noproxy '*' \
    --connect-timeout 2 --max-time 30 --output "$output" --write-out '%{http_code}' \
    "$base_url$path")
  if [ "$status" != 200 ]; then
    echo "read-only report request failed for $path: HTTP $status" >&2
    exit 1
  fi
}
append_json() {
  array_file=$1
  value_file=$2
  next_file="$report_dir/array-next.json"
  jq --slurpfile value "$value_file" '. + [$value[0]]' "$array_file" >"$next_file"
  mv -f -- "$next_file" "$array_file"
}

projects_response="$report_dir/projects-response.json"
projects_report="$report_dir/projects-report.json"
printf '[]\n' >"$projects_report"
request "/api/0/organizations/$organization/projects/" "$projects_response"
jq -e 'type == "array"' "$projects_response" >/dev/null

jq -r '.[].slug' "$projects_response" | while IFS= read -r project; do
  [ -n "$project" ] || continue
  detail="$report_dir/project-$project.json"
  keys="$report_dir/keys-$project.json"
  filters="$report_dir/filters-$project.json"
  item="$report_dir/item-$project.json"
  request "/api/0/projects/$organization/$project/" "$detail"
  request "/api/0/projects/$organization/$project/keys/" "$keys"
  request "/api/0/projects/$organization/$project/filters/" "$filters"
  jq -n --slurpfile detail "$detail" --slurpfile keys "$keys" --slurpfile filters "$filters" '
    ($detail[0]) as $project |
    {
      slug: $project.slug,
      name: $project.name,
      platform: $project.platform,
      keys: ($keys[0] | map({
        name: (.name // .label // ""),
        active: .isActive,
        rateLimit: (.rateLimit // null)
      })),
      filters: ($filters[0] | map({id, active})),
      pii: {
        scrubData: $project.dataScrubber,
        scrubDefaults: $project.dataScrubberDefaults,
        scrubIPAddresses: $project.scrubIPAddresses,
        sensitiveFieldCount: (($project.sensitiveFields // []) | length),
        safeFieldCount: (($project.safeFields // []) | length),
        advancedRulesConfigured: (($project.relayPiiConfig // "") | length > 0)
      }
    }
  ' >"$item"
  append_json "$projects_report" "$item"
done

alerts_report="$report_dir/alerts-report.json"
if [ "$version" = 24.7.1 ]; then
  issue_report="$report_dir/legacy-issue-report.json"
  printf '[]\n' >"$issue_report"
  jq -r '.[].slug' "$projects_response" | while IFS= read -r project; do
    [ -n "$project" ] || continue
    rules="$report_dir/rules-$project.json"
    item="$report_dir/rules-item-$project.json"
    request "/api/0/projects/$organization/$project/rules/" "$rules"
    jq --arg project "$project" 'map({project: $project, name, status, frequency})' "$rules" >"$item"
    jq --slurpfile values "$item" '. + $values[0]' "$issue_report" >"$report_dir/legacy-issue-next.json"
    mv -f -- "$report_dir/legacy-issue-next.json" "$issue_report"
  done
  metric="$report_dir/legacy-metric.json"
  request "/api/0/organizations/$organization/alert-rules/" "$metric"
  jq -n --slurpfile issue "$issue_report" --slurpfile metric "$metric" '{
    adapter: "legacy-24.7.1",
    issueRules: $issue[0],
    metricRules: ($metric[0] | map({name, status, aggregate, timeWindow}))
  }' >"$alerts_report"
else
  detectors="$report_dir/detectors.json"
  workflows="$report_dir/workflows.json"
  request "/api/0/organizations/$organization/detectors/" "$detectors"
  request "/api/0/organizations/$organization/workflows/" "$workflows"
  jq -n --slurpfile detectors "$detectors" --slurpfile workflows "$workflows" '{
    adapter: "modern-26.10.1",
    detectors: ($detectors[0] | map({name, type, enabled})),
    workflows: ($workflows[0] | map({name, enabled}))
  }' >"$alerts_report"
fi

jq -n --arg generatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg version "$version" \
  --arg organization "$organization" --slurpfile projects "$projects_report" \
  --slurpfile alerts "$alerts_report" '{
    generatedAt: $generatedAt,
    version: $version,
    organization: $organization,
    projects: $projects[0],
    alerts: $alerts[0]
  }'
