#!/usr/bin/env bash
# Build the standalone Scalar API reference into the ProperDocs site output.
#
# Mirrors the in-app reference served by internal/api/handlers_docs.go: it
# reuses the exact vendored Scalar bundle and theme stylesheet the app ships,
# so the published docs reference and the in-app reference stay in lockstep
# (same renderer, same M55 palette, same native light/dark toggle).
#
# Run this AFTER `properdocs build`, which produces the site output dir. The
# Pages CI workflow calls it; it is also runnable locally to get a full API
# page that `properdocs build` alone does not produce.
#
# Usage: docs/site/build-api-reference.sh [SITE_DIR]
#   SITE_DIR defaults to docs/site/site (ProperDocs' default output dir).
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "${script_dir}/../.." && pwd)
site_dir=${1:-"${script_dir}/site"}

scalar_js="${repo_root}/web/static/js/scalar-api-reference.min.js"
scalar_css="${repo_root}/web/static/css/scalar-theme.css"
openapi_spec="${repo_root}/internal/api/openapi.yaml"

# Fail loudly if any input is missing rather than emitting a half-built page.
for f in "${scalar_js}" "${scalar_css}" "${openapi_spec}"; do
  if [[ ! -f "${f}" ]]; then
    echo "build-api-reference: required input not found: ${f}" >&2
    exit 1
  fi
done

if [[ ! -d "${site_dir}" ]]; then
  echo "build-api-reference: site dir not found: ${site_dir} (run 'properdocs build' first)" >&2
  exit 1
fi

api_dir="${site_dir}/api"
mkdir -p "${api_dir}"

cp "${scalar_js}" "${api_dir}/scalar-api-reference.min.js"
cp "${scalar_css}" "${api_dir}/scalar-theme.css"
cp "${openapi_spec}" "${api_dir}/openapi.yaml"

# Mirror internal/api/handlers_docs.go: Scalar bundle + Stillwater theme. Asset
# paths are relative so the page works under the GitHub Pages project subpath.
cat > "${api_dir}/index.html" <<'HTML'
<!doctype html>
<html lang="en">
<head>
  <title>API Reference - Stillwater</title>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <link rel="stylesheet" href="scalar-theme.css" />
</head>
<body>
  <script
    id="api-reference"
    data-url="openapi.yaml"
    data-configuration='{"theme":"none","darkMode":true}'
  ></script>
  <script src="scalar-api-reference.min.js"></script>
</body>
</html>
HTML

echo "build-api-reference: wrote ${api_dir}/index.html"
