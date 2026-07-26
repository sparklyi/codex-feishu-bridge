package intent

import (
	"testing"
)

func TestPrivatePlainTextStartsDefaultTask(t *testing.T) {
	got := ParseStart(ParseInput{
		Text:           "fix tests",
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.Prompt != "fix tests" || got.ProjectAlias != "" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestPrivateProjectPrefix(t *testing.T) {
	got := ParseStart(ParseInput{
		Text:           "@backend fix tests",
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.ProjectAlias != "backend" || got.Prompt != "fix tests" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestPrivateUnknownProject(t *testing.T) {
	got := ParseStart(ParseInput{
		Text:           "@missing fix tests",
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindUnknownProject || got.ProjectAlias != "missing" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestSlashPrefixedPromptStartsTask(t *testing.T) {
	got := ParseStart(ParseInput{
		Text:           "/plan fix tests",
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.Prompt != "/plan fix tests" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestBlankTextIsIgnored(t *testing.T) {
	got := ParseStart(ParseInput{Text: "   ", ProjectAliases: []string{"backend"}})
	if got.Kind != KindIgnored {
		t.Fatalf("blank text should be ignored: %+v", got)
	}
}
