package web

import (
	_ "embed"
	"net/http"
)

// openAPIPath serves the hand-written description of /api/. It is tested
// at full route coverage (openapi_test.go): every route, and its scope,
// both ways. A route change edits openapi.json in the same change.
const openAPIPath = "/api/openapi.json"

//go:embed openapi.json
var openAPIDoc []byte

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(openAPIDoc) //nolint:errcheck
}
