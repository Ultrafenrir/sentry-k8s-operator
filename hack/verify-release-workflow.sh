#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
workflow="$root_dir/.github/workflows/release.yml"

ruby -ryaml -e '
  workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
  trigger = workflow["on"] || workflow[true]
  abort "release workflow must run only from pushed tags" unless trigger == {"push" => {"tags" => ["v*"]}}
  abort "top-level permissions must be empty" unless workflow["permissions"] == {}
  job = workflow.fetch("jobs").fetch("image")
  expected = {"contents" => "read", "packages" => "write", "id-token" => "write"}
  abort "release permissions are not least privilege" unless job["permissions"] == expected
  expected_actions = [
    "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd",
    "docker/setup-qemu-action@29109295f81e9208d7d86ff1c6c12d2833863392",
    "docker/setup-buildx-action@e468171a9de216ec08956ac3ada2f0791b6bd435",
    "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6",
  ]
  actions = job.fetch("steps").map { |step| step["uses"] }.compact
  abort "release actions or their commit pins changed" unless actions == expected_actions
  abort "release action is not pinned by full commit SHA" unless actions.all? { |use| use.match?(/@[0-9a-f]{40}\z/) }
' "$workflow"
rg -Fq 'persist-credentials: false' "$workflow"
rg -Fq 'cosign-release: v3.1.4' "$workflow"
rg -Fq 'make docker-buildx IMG="$RELEASE_TAG"' "$workflow"
rg -Fq 'digest=$(jq -er' "$workflow"
rg -Fq 'cosign sign --yes "${repository}@${digest}"' "$workflow"
rg -Fq 'identity="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/.github/workflows/release.yml@${GITHUB_REF}"' "$workflow"
rg -Fq 'make verify-signed-image IMG="${repository}@${digest}" COSIGN_CERTIFICATE_IDENTITY="$identity"' "$workflow"
rg -Fq 'REGISTRY_TOKEN: ${{ secrets.GITHUB_TOKEN }}' "$workflow"
! rg -q 'pull_request|workflow_dispatch|branches:' "$workflow"

echo 'Pinned tag-only Buildx, keyless cosign signing and exact self-verification workflow contract verified'
