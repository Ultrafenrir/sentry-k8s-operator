#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=sentry-operator-t01-validation
project_crd=sentryprojects.sentryconfiguration.io
connection_crd=sentryconnections.sentryconfiguration.io
tmp_dir=$(mktemp -d)

cleanup() {
  kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl --context "$context" delete crd "$project_crd" "$connection_crd" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

if kubectl --context "$context" get crd "$project_crd" >/dev/null 2>&1 || \
    kubectl --context "$context" get crd "$connection_crd" >/dev/null 2>&1; then
  echo 'refusing to replace pre-existing product CRDs in the selected context' >&2
  exit 1
fi

kubectl --context "$context" apply -f "$root_dir/config/crd/bases" >/dev/null
kubectl --context "$context" create namespace "$namespace" >/dev/null

golden_count=$(find "$root_dir/testdata/t01/golden" -type f -name '*.yaml' | wc -l | tr -d ' ')
test "$golden_count" = 6

for fixture in "$root_dir"/testdata/t01/golden/*.yaml; do
  rendered="$tmp_dir/$(basename "$fixture")"
  sed "s/namespace: .*/namespace: $namespace/" "$fixture" >"$rendered"
  kubectl --context "$context" create --dry-run=server --validate=strict \
    -f "$rendered" -o json >"$rendered.json"
done

jq -e '.spec.paused == false and .spec.lifecycle.onDelete == "Retain"' \
  "$tmp_dir/01-managed-minimal.yaml.json" >/dev/null
jq -e '
  .spec.settings.filters.localhost == false and
  .spec.settings.filters.filteredTransactions == true and
  (.spec.settings.filters.legacyBrowsers | sort) == ["chrome", "ie"] and
  .spec.settings.pii.scrubIPAddresses == false and
  (.spec.settings.pii.safeFields | type == "array" and length == 0) and
  (.spec.settings.pii.sensitiveFields | length == 2) and
  .spec.settings.pii.advancedRules[0].type == "email" and
  .spec.settings.pii.advancedRules[0].applications[1].path == ["customer", "email"] and
  .spec.settings.pii.advancedRules[1].replacement == "[Filtered]" and
  .spec.settings.behavior.autoResolveHours == 72 and
  .spec.settings.behavior.defaultEnvironment == "production" and
  .spec.settings.behavior.crashReportLimit == 10 and
  .spec.settings.behavior.scrapeJavaScript == true and
  (.spec.settings.behavior.allowedDomains | sort) == ["*.checkout.example.com", "https://checkout.example.com:443"] and
  .spec.settings.grouping.enhancements[0].action == "MarkOutOfApp" and
  .spec.settings.grouping.enhancements[1].negate == true and
  .spec.settings.grouping.fingerprints[0].values == ["database-unavailable"] and
  .spec.settings.grouping.fingerprints[1].includeDefault == true and
  (.spec.settings.symbolication.builtinSources | sort) == ["electron", "microsoft", "nuget"] and
  .spec.issueOwnership.fallthrough == "NoOne" and
  .spec.issueOwnership.autoAssignment == "IssueOwner" and
  .spec.issueOwnership.codeownersAutoSync == true and
  .spec.issueOwnership.rules[0].matcher.type == "path" and
  (.spec.issueOwnership.rules[0].teams | sort) == ["payments", "platform-observability"] and
  .spec.issueOwnership.rules[1].matcher.tagKey == "environment" and
  .spec.clientKeys.ingest.active == true and
  .spec.clientKeys.ingest.rotationRevision == 1 and
  .spec.clientKeys.ingest.rateLimit.count == 1000 and
  .spec.clientKeys.ingest.rateLimit.windowSeconds == 60 and
  .spec.symbolSources["http-symbols"].http.url == "https://symbols.example.invalid" and
  .spec.symbolSources["http-symbols"].http.passwordSecretRef.key == "http-password" and
  .spec.symbolSources["s3-symbols"].s3.region == "eu-west-1" and
  .spec.symbolSources["s3-symbols"].s3.secretKeySecretRef.key == "s3-secret-key" and
  .spec.symbolSources["gcs-symbols"].gcs.clientEmail == "symbols@example.invalid" and
  .spec.symbolSources["gcs-symbols"].gcs.privateKeySecretRef.key == "gcs-private-key" and
  .spec.dsn.managedClientKey == "ingest"
' "$tmp_dir/04-managed-settings.yaml.json" >/dev/null
jq -e '
  .spec.alerts["new-errors"].trigger == "first-seen" and
  .spec.alerts["new-errors"].enabled == true and
  .spec.alerts["error-volume"].windowMinutes == 5 and
  .spec.alerts["error-volume"].threshold == 200 and
  .spec.alerts["affected-users"].trigger == "user-count" and
  .spec.alerts["affected-users"].windowMinutes == 10 and
  .spec.alerts["affected-users"].threshold == 50
' "$tmp_dir/06-managed-alerts.yaml.json" >/dev/null

for fixture in "$root_dir"/testdata/t01/negative/*.yaml; do
	case "$(basename "$fixture")" in
	multiple-destination-types.yaml)
		# Duplicate terminal names across provider catalogs require webhook validation.
		continue
		;;
	esac
  rendered="$tmp_dir/negative-$(basename "$fixture")"
  sed "s/namespace: .*/namespace: $namespace/" "$fixture" >"$rendered"
  if kubectl --context "$context" create --dry-run=server --validate=strict \
    -f "$rendered" >"$rendered.out" 2>&1; then
    echo "negative fixture unexpectedly passed: $(basename "$fixture")" >&2
    exit 1
  fi
done

echo "$context: six T01 golden fixtures and all schema/CEL negatives passed"
