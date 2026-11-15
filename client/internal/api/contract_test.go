package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type example struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Query    json.RawMessage `json:"query"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

func loadExample(t *testing.T, name string) example {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../../contract/api", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var e example
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func strict(t *testing.T, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatal(err)
	}
}

// Every response example decodes into its type with unknown fields refused, so a key
// added by the service without the client knowing fails here.
func TestEveryResponseExampleDecodes(t *testing.T) {
	types := map[string]func() any{
		"me":                func() any { return &Me{} },
		"error":             func() any { return &errorBody{} },
		"grants-create":     func() any { return &GrantResponse{} },
		"grants-get":        func() any { return &GrantDetail{} },
		"grants-list":       func() any { return &GrantList{} },
		"grants-approve":    func() any { return &GrantResponse{} },
		"grants-reject":     func() any { return &GrantResponse{} },
		"grants-cancel":     func() any { return &GrantResponse{} },
		"grants-revoke":     func() any { return &GrantResponse{} },
		"grants-redrive":    func() any { return &GrantResponse{} },
		"rules-list":        func() any { return &RuleList{} },
		"rules-permissions": func() any { return &Permissions{} },
		"rules-check":       func() any { return &CheckResult{} },
		"config-versions":   func() any { return &ConfigVersions{} },
		"config-version":    func() any { return &ConfigVersion{} },
	}
	files, err := filepath.Glob("../../../contract/api/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(types) {
		t.Fatalf("%d examples, %d types: every example needs a type", len(files), len(types))
	}
	for _, f := range files {
		name := filepath.Base(f[:len(f)-len(".json")])
		build, ok := types[name]
		if !ok {
			t.Fatalf("no type for contract/api/%s.json", name)
		}
		strict(t, loadExample(t, name).Response, build())
	}
}

func TestRequestTypesEncodeLikeTheExamples(t *testing.T) {
	cases := map[string]any{
		"grants-create": CreateRequest{RuleID: "ssm-prod", Account: "222222222222", DurationSeconds: 3600, Reason: "incident 42"},
		"grants-revoke": revokeRequest{Reason: "incident closed"},
	}
	for name, req := range cases {
		got, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var want, have any
		if err := json.Unmarshal(loadExample(t, name).Request, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &have); err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(want, have) {
			t.Errorf("%s: got %s, want %s", name, got, loadExample(t, name).Request)
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestVocabularyMatchesTheContract(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/vocabulary.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		States       []string `json:"states"`
		CloseReasons []string `json:"closeReasons"`
		ErrorCodes   []string `json:"errorCodes"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	same := func(what string, got []string, want []string) {
		if !jsonEqual(got, want) {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	same("states", States, v.States)
	same("close reasons", CloseReasons, v.CloseReasons)
	same("error codes", ErrorCodes, v.ErrorCodes)
}
