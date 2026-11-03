package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWithModelBytewise(t *testing.T) {
	cases := []string{
		`{"model":"auto","messages":[{"role":"user","content":"x \"model\":\"y\" {"}]}`,
		` { "messages" : [ {"content":"a\\\"}"} ], "model" : "auto" , "stream": true }`,
		`{"stream":true,"model":null}`,
		`{"messages":[],"nested":{"model":"keep"}}`,
		`{}`,
		`{"model":"escaped key","n":1.5e3,"b":false}`,
	}
	for _, c := range cases {
		out := withModel([]byte(c), `qwen3:8b "q"`)
		var in, got map[string]any
		json.Unmarshal([]byte(c), &in)
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s → invalid %s: %v", c, out, err)
		}
		in["model"] = `qwen3:8b "q"`
		if !reflect.DeepEqual(in, got) {
			t.Errorf("%s → %s", c, out)
		}
	}
	if string(withModel([]byte(`not json`), "x")) != "not json" {
		t.Error("non-JSON body changed")
	}
	// untouched parts stay byte for byte
	orig := `{"model":"auto",  "messages":[{"content":"ä ä"}]}`
	if got := string(withModel([]byte(orig), "m")); got != `{"model":"m",  "messages":[{"content":"ä ä"}]}` {
		t.Errorf("got %s", got)
	}
}

func TestWithStreamUsageBytewise(t *testing.T) {
	out, ok := withStreamUsage([]byte(` {}`))
	var m map[string]any
	if !ok || json.Unmarshal(out, &m) != nil || m["stream_options"] == nil {
		t.Errorf("empty object: %s", out)
	}
	if _, ok := withStreamUsage([]byte(`{"a":{"stream_options":1}}`)); !ok {
		t.Error("nested key is not the top-level one")
	}
}

func TestStripAndNormalize(t *testing.T) {
	if got := stripReminders("a<system-reminder>x</system-reminder>b<system-reminder>y</system-reminder>c<system-reminder>offen"); got != "abc<system-reminder>offen" {
		t.Errorf("strip: %q", got)
	}
	if p, n := normPrefix("  ein \n\t text  mit   leerraum ", 100); p != "ein text mit leerraum" || n != len(p) {
		t.Errorf("norm: %q %d", p, n)
	}
	if p, n := normPrefix(strings.Repeat("ä", 500), 300); n != 300 || len([]rune(p)) != 300 {
		t.Errorf("limit: %d", n)
	}
	long := strings.Repeat("Ergebnis der Prüfung ", 20)
	if msgKey("<think>lang</think>  "+long) != msgKey(long) || msgKey("<think>noch am denken "+long) != 0 || msgKey("kurz") != 0 {
		t.Error("msgKey")
	}
}
