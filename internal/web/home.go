package web

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"

	"github.com/chrisbodhi/blygd/internal/config"
)

//go:embed home.html
var homeHTML string

var homeTmpl = template.Must(template.New("home").Parse(homeHTML))

// homeCSP: the page is text and links. It loads nothing, runs nothing,
// and nobody may frame it.
const homeCSP = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// HomePage renders the page at the host root: what this service is, where
// its API is described (with rel="service-desc", as on the studio's page,
// §16.6), its source, and the operator's own links, each rel="me" so a
// profile that links back here can verify it. Like llms.txt it names no
// origin and lists no users. It is headed with the instance's name, and
// says it runs blygd.
func HomePage(name, source string, operator []config.Link) []byte {
	var b bytes.Buffer
	if err := homeTmpl.Execute(&b, struct {
		Name     string
		Source   string
		Operator []config.Link
	}{instanceName(name), source, operator}); err != nil {
		panic(err) // the template is embedded, and its data are strings
	}
	return b.Bytes()
}

// instanceName is what this instance is called (config.Name), or blygd,
// the software's own name, when nothing says.
func instanceName(name string) string {
	if name == "" {
		return "blygd"
	}
	return name
}

// serveHome writes the host root page. Surface calls it only when no
// origin is mounted at the root, so it shadows nothing.
func serveHome(w http.ResponseWriter, page []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", homeCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Cache-Control", "public, max-age=3600")
	w.Write(page) //nolint:errcheck
}
