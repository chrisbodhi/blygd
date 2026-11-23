// Command gendoc writes the copy of internal/web/openapi.json that the
// Swift SDK's generator reads (scripts/swift-sdk.sh):
//
//	go run ./sdk/swift/gendoc internal/web/openapi.json out.json
//
// The copy differs in one way. Each schema in [overridden] is replaced
// with an empty schema that keeps only its description. Those are the
// schemas the generator cannot model, which it would otherwise drop along
// with every property that refers to them. openapi-generator-config.yaml
// maps each one to a hand-written Swift type (typeOverrides), but the
// generator checks support before it applies an override, so the override
// alone does not save the property.
//
// The rest of the document is copied byte for byte, so the generated code
// keeps the document's order.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// overridden lists the schemas replaced by hand-written Swift types. Each
// needs a typeOverrides entry in openapi-generator-config.yaml, and a type
// in sdk/swift/Sources/Blygd (TestOverridesAreConfigured).
var overridden = map[string]string{
	// oneOf null | ItemDocument | L0Document: the generator has no
	// {"type": "null"} branch.
	"ImportedDoc": "ImportedDocument",
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gendoc <openapi.json> <out.json>")
		os.Exit(2)
	}
	doc, err := os.ReadFile(os.Args[1])
	if err == nil {
		doc, err = prepare(doc)
	}
	if err == nil {
		err = os.WriteFile(os.Args[2], doc, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendoc:", err)
		os.Exit(1)
	}
}

func prepare(doc []byte) ([]byte, error) {
	for name := range overridden {
		start, end, err := locate(doc, "components", "schemas", name)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", name, err)
		}
		var schema struct {
			Description string `json:"description,omitempty"`
		}
		if err := json.Unmarshal(doc[start:end], &schema); err != nil {
			return nil, fmt.Errorf("schema %s: %w", name, err)
		}
		empty, err := json.Marshal(schema)
		if err != nil {
			return nil, err
		}
		doc = append(doc[:start:start], append(empty, doc[end:]...)...)
	}
	return doc, nil
}

// locate returns the byte range of the value at path, a chain of object
// keys from the document's root.
func locate(doc []byte, path ...string) (start, end int, err error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	for _, key := range path {
		if t, err := dec.Token(); err != nil {
			return 0, 0, err
		} else if t != json.Delim('{') {
			return 0, 0, fmt.Errorf("%q is inside %v, not an object", key, t)
		}
		for {
			if !dec.More() {
				return 0, 0, fmt.Errorf("no %q", key)
			}
			k, err := dec.Token()
			if err != nil {
				return 0, 0, err
			}
			if k == key {
				break
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, err
			}
		}
	}
	// The decoder stops after the key; the value starts past the colon.
	start = int(dec.InputOffset())
	for start < len(doc) && bytes.IndexByte([]byte(": \t\r\n"), doc[start]) >= 0 {
		start++
	}
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return 0, 0, err
	}
	end = int(dec.InputOffset())
	if !bytes.Equal(doc[start:end], value) {
		return 0, 0, fmt.Errorf("located %q, not the value", doc[start:min(end, start+20)])
	}
	return start, end, nil
}
