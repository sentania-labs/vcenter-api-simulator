#!/usr/bin/env bash
set -euo pipefail
chart=charts/vcenter-api-simulator
scratch=$(mktemp -d "${TMPDIR:-${RUNNER_TEMP:-/tmp}}/vcsim-chart.XXXXXXXX")
trap 'rm -rf "$scratch"' EXIT
helm template test "$chart" --set ingress.enabled=true --set hostname=example.test > "$scratch/default.yaml"
for path in /sdk /vsanHealth /lookupservice /sso-adminserver /sts /ls /api /rest /stats /wcp; do
  grep -Fq "path: $path" "$scratch/default.yaml"
done
helm template test "$chart" --set ingress.enabled=true --set hostname=example.test --set 'ingress.paths[0]=/custom' > "$scratch/override.yaml"
grep -Fq 'path: /custom' "$scratch/override.yaml"
if grep -Fq 'path: /sdk' "$scratch/override.yaml"; then exit 1; fi
version=$(helm show chart "$chart" | awk '$1 == "version:" {print $2}')
helm package "$chart" --version "$version" --app-version "$version" --destination "$scratch" >/dev/null
helm template test "$scratch/vcenter-api-simulator-$version.tgz" > "$scratch/release.yaml"
grep -Fq ":v$version\"" "$scratch/release.yaml"
helm template test "$chart" --set image.tag=custom > "$scratch/image.yaml"
grep -Fq ':custom"' "$scratch/image.yaml"
# Render the install notes as a ConfigMap so Helm exposes the generated text.
cp -R "$chart" "$scratch/chart"
{
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes-test\ndata:\n  notes: |-\n'
  sed 's/^/    /' "$chart/templates/NOTES.txt"
} > "$scratch/chart/templates/notes-test.yaml"
helm template test "$scratch/chart" > "$scratch/notes.yaml"
grep -Fq 'charts/vcenter-api-simulator#readme' "$scratch/notes.yaml"
