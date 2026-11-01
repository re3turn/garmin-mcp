package main

import (
	"encoding/json"
	"testing"
)

func TestUnquoteJS(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`'plain' rest`, "plain"},
		{`'it\'s'`, "it's"},
		{`"say \"hi\""`, `say "hi"`},
		{`'a\\b'`, `a\b`},
		{`'\n\t'`, "\n\t"},
		{`'\x41\u0042\u{43}'`, "ABC"},
		{`'\ud83c\udfc3'`, "🏃"},
		{`'日本'`, "日本"},
		{"'a\\\nb'", "ab"},
		{"'a\\\r\nb'", "ab"},
		{`'\u{1f3c3}'`, "🏃"},
		{`'\0'`, "\x00"},
	}
	for _, tt := range tests {
		got, err := unquoteJS(tt.in)
		if err != nil {
			t.Errorf("unquoteJS(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("unquoteJS(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnquoteJSErrors(t *testing.T) {
	for _, in := range []string{``, `abc`, `'abc`, "'a\nb'", `'\x4'`, `'\u12'`, `'\u{110000}'`, `'\ud83c'`, `'\udfc3\ud83c'`, `'\01'`} {
		if _, err := unquoteJS(in); err == nil {
			t.Errorf("unquoteJS(%q): want error", in)
		}
	}
}

func TestExtractSpec(t *testing.T) {
	js := `x={a:1},wOaG:function(e){e.exports=JSON.parse('{"openapi":"3.0.1","info":{"title":"Women\'s Health","path":"a\\\\b"}}')},y=2`
	got, err := extractSpec(js)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title string `json:"title"`
			Path  string `json:"path"`
		} `json:"info"`
	}
	if err := json.Unmarshal(got, &spec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, got)
	}
	if spec.OpenAPI != "3.0.1" || spec.Info.Title != "Women's Health" || spec.Info.Path != `a\b` {
		t.Errorf("unexpected spec: %+v", spec)
	}
	if got[len(got)-1] != '\n' {
		t.Error("output does not end with a newline")
	}
}

func TestExtractSpecDoubleQuoted(t *testing.T) {
	got, err := extractSpec(`e.exports=JSON.parse("{\"openapi\":\"3.0.1\"}")`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"openapi\": \"3.0.1\"\n}\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChunkURL(t *testing.T) {
	const page = "https://developer.garmin.com/gc-developer-program/personal/swagger-docs/"
	const name = "/component---src-pages-gc-developer-program-personal-swagger-docs-js-0123abcd.js"
	tests := []struct {
		html, want string
	}{
		{`<script src="` + name + `" async>`, "https://developer.garmin.com" + name},
		{`<script src="/prefix` + name + `">`, "https://developer.garmin.com/prefix" + name},
		{`<link href='https://cdn.example.com` + name + `'>`, "https://cdn.example.com" + name},
	}
	for _, tt := range tests {
		got, err := chunkURL(page, []byte(tt.html))
		if err != nil {
			t.Errorf("chunkURL(%q): %v", tt.html, err)
			continue
		}
		if got != tt.want {
			t.Errorf("chunkURL(%q) = %q, want %q", tt.html, got, tt.want)
		}
	}
	if _, err := chunkURL(page, []byte("<html></html>")); err == nil {
		t.Error("chunkURL without a chunk: want error")
	}
}

func TestExtractSpecCount(t *testing.T) {
	for _, js := range []string{
		`no spec here`,
		`JSON.parse('{"openapi":"3"}');JSON.parse('{"openapi":"3"}')`,
		`JSON.parse('{"openapi":"3"}');JSON.parse("{\"openapi\":\"3\"}")`,
	} {
		if _, err := extractSpec(js); err == nil {
			t.Errorf("extractSpec(%q): want error", js)
		}
	}
}
