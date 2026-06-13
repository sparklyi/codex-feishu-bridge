package router

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantAlias  string
		wantPrompt string
		wantOK     bool
	}{
		{name: "default", text: "/codex hello", wantPrompt: "hello", wantOK: true},
		{name: "alias", text: "/codex @backend fix", wantAlias: "backend", wantPrompt: "fix", wantOK: true},
		{name: "not codex", text: "/not-codex hello"},
		{name: "empty", text: "/codex"},
		{name: "alias empty", text: "/codex @backend"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alias, prompt, ok := ParseCommand(tt.text)
			if ok != tt.wantOK || alias != tt.wantAlias || prompt != tt.wantPrompt {
				t.Fatalf("ParseCommand(%q) = alias=%q prompt=%q ok=%v", tt.text, alias, prompt, ok)
			}
		})
	}
}
