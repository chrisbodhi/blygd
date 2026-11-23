package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

func TestPrepareReplacesOnlyTheOverriddenSchemas(t *testing.T) {
	doc, err := os.ReadFile("../../../internal/web/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := prepare(bytes.Clone(doc))
	if err != nil {
		t.Fatal(err)
	}

	var want, have map[string]any
	if err := json.Unmarshal(doc, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatalf("prepared document is not JSON: %v", err)
	}
	schemas := want["components"].(map[string]any)["schemas"].(map[string]any)
	for name := range overridden {
		old, ok := schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("openapi.json has no schema %s: drop it from overridden", name)
		}
		schemas[name] = map[string]any{"description": old["description"]}
	}
	if !reflect.DeepEqual(have, want) {
		t.Error("prepare changed more than the overridden schemas")
	}

	// Byte for byte up to the first splice, so the order is the document's.
	start, _, err := locate(doc, "components", "schemas", "ImportedDoc")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:start], doc[:start]) {
		t.Error("prepare rewrote bytes before the splice")
	}
}

func TestLocate(t *testing.T) {
	doc := []byte(`{"a": "}{\"", "components": {"x": {"schemas": 1},
	  "schemas": {"B": [1, {"c": "}"}], "ImportedDoc":  {"description": "d", "oneOf": [{"type": "null"}]}, "Z": 2}}}`)
	start, end, err := locate(doc, "components", "schemas", "ImportedDoc")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(doc[start:end]); got != `{"description": "d", "oneOf": [{"type": "null"}]}` {
		t.Errorf("located %q", got)
	}
	if _, _, err := locate(doc, "components", "schemas", "Missing"); err == nil {
		t.Error("located a schema that is not there")
	}
	if _, _, err := locate(doc, "a", "b"); err == nil {
		t.Error("located a key inside a string")
	}
}

// Each scope gets a Swift name chosen on purpose, not `items_colon_read`:
// renaming a case after it has shipped breaks the apps that match on it.
func TestScopesAreNamed(t *testing.T) {
	doc, err := os.ReadFile("../../../internal/web/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile("../openapi-generator-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components struct {
			Schemas struct {
				Scope struct {
					Enum []string `json:"enum"`
				} `json:"Scope"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	scopes := d.Components.Schemas.Scope.Enum
	if len(scopes) == 0 {
		t.Fatal("openapi.json has no Scope enum")
	}
	for _, s := range scopes {
		line := regexp.MustCompile(`(?m)^\s+"` + regexp.QuoteMeta(s) + `":\s*[a-z][A-Za-z]*\s*$`)
		if !line.Match(config) {
			t.Errorf("openapi-generator-config.yaml: no nameOverrides line for scope %q", s)
		}
	}
}

// Each overridden schema needs its typeOverrides entry and its Swift type,
// or the generator emits a property whose type does not exist. And each
// typeOverrides entry needs its line in overridden: an override the
// generator cannot see past is one it silently drops.
func TestOverridesAreConfigured(t *testing.T) {
	config, err := os.ReadFile("../openapi-generator-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := bytes.Cut(config, []byte("\ntypeOverrides:\n  schemas:\n"))
	if !ok {
		t.Fatal("openapi-generator-config.yaml: no typeOverrides.schemas block")
	}
	configured := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^    ([A-Za-z0-9_]+):\s*([A-Za-z0-9_.]+)\s*$`).FindAllSubmatch(block, -1) {
		configured[string(m[1])] = string(m[2])
	}
	if !reflect.DeepEqual(configured, overridden) {
		t.Errorf("typeOverrides.schemas is %v; gendoc's overridden is %v: keep them the same", configured, overridden)
	}
	sources, err := filepath.Glob("../Sources/Blygd/*.swift")
	if err != nil {
		t.Fatal(err)
	}
	var swift []byte
	for _, f := range sources {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		swift = append(swift, b...)
	}
	for schema, typ := range overridden {
		entry := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(schema) + `:\s*` + regexp.QuoteMeta(typ) + `\s*$`)
		if !entry.Match(config) {
			t.Errorf("openapi-generator-config.yaml: no typeOverrides entry %s: %s", schema, typ)
		}
		decl := regexp.MustCompile(`(?m)^public (enum|struct) ` + regexp.QuoteMeta(typ) + `\b`)
		if !decl.Match(swift) {
			t.Errorf("sdk/swift/Sources/Blygd declares no public type %s", typ)
		}
	}
}
