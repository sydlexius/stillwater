package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var openapiSpec []byte

func (r *Router) handleAPIDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<head>
  <title>API Reference - Stillwater</title>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <link rel="icon" type="image/svg+xml" href="` + r.basePath + r.staticAssets.Path("/img/favicon.svg") + `" />
  <link rel="icon" type="image/png" sizes="32x32" href="` + r.basePath + r.staticAssets.Path("/img/favicon-32x32.png") + `" />
  <link rel="stylesheet" href="` + r.basePath + r.staticAssets.Path("/css/scalar-theme.css") + `" />
</head>
<body>
  <script
    id="api-reference"
    data-url="` + r.basePath + `/api/v1/docs/openapi.yaml"
    data-configuration='{"theme":"none","darkMode":true}'
  ></script>
  <script src="` + r.basePath + r.staticAssets.Path("/js/scalar-api-reference.min.js") + `"></script>
</body>
</html>`))
}

func (r *Router) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(openapiSpec)
}
