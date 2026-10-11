package web

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type openAPIOp struct {
	Security *[]map[string][]string `json:"security"`
}

func loadOpenAPI(t *testing.T) (map[string]map[string]json.RawMessage, map[string]any) {
	t.Helper()
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDoc, &doc); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Fatalf("openapi %q: scoped bearer requirements need 3.1", doc.OpenAPI)
	}
	var whole map[string]any
	_ = json.Unmarshal(openAPIDoc, &whole)
	return doc.Paths, whole
}

var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// The description must match the router exactly: every route documented,
// nothing documented that does not exist, and the same scope on both sides
// (docs/PLAN.md, Operations).
func TestOpenAPICoversEveryRoute(t *testing.T) {
	paths, _ := loadOpenAPI(t)

	want := map[string]string{"GET " + openAPIPath: "public"}
	for _, r := range (&API{}).routes() {
		want[r.pattern] = r.scope
	}
	got := map[string]string{}
	for path, item := range paths {
		for method, raw := range item {
			if !slices.Contains(httpMethods, method) {
				continue // "parameters", "summary", …
			}
			var op openAPIOp
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			key := strings.ToUpper(method) + " " + path
			switch {
			case op.Security == nil:
				t.Errorf("%s: no security requirement; say the scope, or [] for public", key)
			case len(*op.Security) == 0:
				got[key] = "public"
			case len(*op.Security) != 1 || len((*op.Security)[0]["bearer"]) > 1:
				t.Errorf("%s: want exactly one bearer requirement with at most one scope, got %v", key, *op.Security)
			case len((*op.Security)[0]["bearer"]) == 0:
				got[key] = ""
			default:
				got[key] = (*op.Security)[0]["bearer"][0]
			}
		}
	}
	for route, scope := range want {
		docScope, ok := got[route]
		switch {
		case !ok:
			t.Errorf("route %s is not in openapi.json", route)
		case docScope != scope:
			t.Errorf("%s: scope is %q in code, %q in openapi.json", route, scope, docScope)
		}
	}
	for route := range got {
		if _, ok := want[route]; !ok {
			t.Errorf("openapi.json documents %s, which is not a route", route)
		}
	}
}

// Every $ref points at something that exists, and every path parameter is
// declared.
func TestOpenAPIIsSelfConsistent(t *testing.T) {
	paths, whole := loadOpenAPI(t)
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				var at any = whole
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					m, _ := at.(map[string]any)
					at = m[part]
				}
				if at == nil {
					t.Errorf("dangling $ref %s", ref)
				}
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(whole)

	params := whole["components"].(map[string]any)["parameters"].(map[string]any)
	for path, item := range paths {
		declared := map[string]bool{}
		var refs []any
		_ = json.Unmarshal(item["parameters"], &refs)
		for _, r := range refs {
			name := strings.TrimPrefix(r.(map[string]any)["$ref"].(string), "#/components/parameters/")
			if p, ok := params[name].(map[string]any); ok && p["in"] == "path" {
				declared[p["name"].(string)] = true
			}
		}
		for _, seg := range strings.Split(path, "/") {
			if strings.HasPrefix(seg, "{") && !declared[strings.Trim(seg, "{}")] {
				t.Errorf("%s: path parameter %s is not declared", path, seg)
			}
		}
	}
}

func TestOpenAPIIsServedWithoutAToken(t *testing.T) {
	rec := httptest.NewRecorder()
	(&API{}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", openAPIPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
		rec.Body.String() != string(openAPIDoc) {
		t.Fatalf("GET %s: %d %v", openAPIPath, rec.Code, rec.Header())
	}
}

// ── response shapes ──────────────────────────────────────────────────────

// checkResponse validates an /api/ response body against the schema
// openapi.json documents for that operation and status. It checks shape,
// not every keyword: required members present, no undocumented members
// where additionalProperties is false, arrays, oneOf, and JSON types. The
// e2e tests call it on every API response, so a renamed or added field
// fails until the document follows. A body that is not JSON must carry a
// media type the document lists for that status.
func checkResponse(t *testing.T, method, path string, status int, ctype string, body []byte) {
	t.Helper()
	if path == openAPIPath {
		return
	}
	var doc map[string]any
	_ = json.Unmarshal(openAPIDoc, &doc)
	pattern, item := matchPath(doc["paths"].(map[string]any), path)
	if item == nil {
		t.Errorf("openapi: no path matches %s %s", method, path)
		return
	}
	op, _ := item[strings.ToLower(method)].(map[string]any)
	if op == nil {
		t.Errorf("openapi: %s %s is not documented", method, pattern)
		return
	}
	resp, _ := op["responses"].(map[string]any)[strconv.Itoa(status)].(map[string]any)
	if resp == nil {
		t.Errorf("openapi: %s %s does not document status %d", method, pattern, status)
		return
	}
	resp = deref(doc, resp)
	if len(body) == 0 {
		return
	}
	content, _ := resp["content"].(map[string]any)
	if content == nil {
		t.Errorf("openapi: %s %s %d documents no body, got %s", method, pattern, status, body)
		return
	}
	mt, _, _ := mime.ParseMediaType(ctype)
	if mt != "application/json" {
		if content[mt] == nil {
			t.Errorf("openapi: %s %s %d: %q is not a documented media type", method, pattern, status, ctype)
		}
		return
	}
	schema := content["application/json"].(map[string]any)["schema"].(map[string]any)
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Errorf("%s %s %d: not JSON", method, pattern, status)
		return
	}
	if msg := conforms(doc, schema, v, "$"); msg != "" {
		t.Errorf("openapi: %s %s %d: %s", method, pattern, status, msg)
	}
}

func matchPath(paths map[string]any, path string) (string, map[string]any) {
	segs := strings.Split(path, "/")
	for pattern, item := range paths {
		ps := strings.Split(pattern, "/")
		if len(ps) != len(segs) {
			continue
		}
		ok := true
		for i := range ps {
			if !strings.HasPrefix(ps[i], "{") && ps[i] != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return pattern, item.(map[string]any)
		}
	}
	return "", nil
}

func deref(doc map[string]any, s map[string]any) map[string]any {
	for {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		var at any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			at = at.(map[string]any)[part]
		}
		// A $ref beside a description keeps the referenced schema.
		s = at.(map[string]any)
	}
}

func jsonType(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}

// conforms returns "" or the first mismatch, located by a JSON path.
func conforms(doc, s map[string]any, v any, at string) string {
	s = deref(doc, s)
	if alts, ok := s["oneOf"].([]any); ok {
		var msgs []string
		for _, alt := range alts {
			msg := conforms(doc, alt.(map[string]any), v, at)
			if msg == "" {
				return ""
			}
			msgs = append(msgs, msg)
		}
		return at + ": matches no oneOf branch (" + strings.Join(msgs, "; ") + ")"
	}
	if t, ok := s["type"]; ok {
		var types []string
		switch t := t.(type) {
		case string:
			types = []string{t}
		case []any:
			for _, x := range t {
				types = append(types, x.(string))
			}
		}
		got := jsonType(v)
		if !slices.Contains(types, got) && !(got == "integer" && slices.Contains(types, "number")) {
			return fmt.Sprintf("%s: %s, want %v", at, got, types)
		}
	}
	switch v := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range asStrings(s["required"]) {
			if _, ok := v[r]; !ok {
				return at + ": missing required " + r
			}
		}
		for k, val := range v {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if s["additionalProperties"] == false {
					return at + ": undocumented member " + k
				}
				continue
			}
			if msg := conforms(doc, ps, val, at+"."+k); msg != "" {
				return msg
			}
		}
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range v {
				if msg := conforms(doc, items, x, fmt.Sprintf("%s[%d]", at, i)); msg != "" {
					return msg
				}
			}
		}
	}
	return ""
}

func asStrings(v any) []string {
	var out []string
	xs, _ := v.([]any)
	for _, x := range xs {
		out = append(out, x.(string))
	}
	return out
}
