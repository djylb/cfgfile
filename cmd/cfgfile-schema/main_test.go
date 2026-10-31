package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	b, err := generate("testdata/app", "Config", "https://example.com/app-1.json", "version")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		ID         string                     `json:"$id"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	var names []string
	for k := range s.Properties {
		names = append(names, k)
	}
	if len(s.Properties) != 14 || s.ID == "" || s.Defs["Log"] == nil {
		t.Fatalf("%v\n%s", names, b)
	}
	for _, want := range []string{
		`"description": "data directory"`,
		`"description": "Log level.",`,
		`"enum": [`,
		`"default": "info"`,
		`"default": [`,
		`"$ref": "#/$defs/Log"`,
		`"additionalProperties": false`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"names":{"type":["array","null"]`,
		`"backup":{"anyOf":[{"$ref":"#/$defs/Log"},{"type":"null"}]}`,
		`"pair":{"type":"array","items":{"type":"integer"},"maxItems":2}`,
		`"rev":{"type":"integer"}`,
	} {
		if !strings.Contains(compact.String(), want) {
			t.Errorf("missing %s in\n%s", want, compact.String())
		}
	}
	nv, _ := generate("testdata/app", "NoVersion", "", "ver")
	if !strings.Contains(string(nv), `"ver": {
      "type": "integer",
      "minimum": 1`) {
		t.Errorf("version member not allowed:\n%s", nv)
	}
	if _, err := generate("testdata/app", "Nope", "", "version"); err == nil {
		t.Fatal("missing type accepted")
	}
}
