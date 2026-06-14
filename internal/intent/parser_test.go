package intent

import (
	"testing"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestPrivatePlainTextStartsDefaultTask(t *testing.T) {
	got := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "private", Text: "fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.Prompt != "fix tests" || got.ProjectAlias != "" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestPrivateProjectPrefix(t *testing.T) {
	got := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "private", Text: "@backend fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindStartTask || got.ProjectAlias != "backend" || got.Prompt != "fix tests" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestPrivateUnknownProject(t *testing.T) {
	got := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "private", Text: "@missing fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindUnknownProject || got.ProjectAlias != "missing" {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestCodexCommandReturnsMigrationHint(t *testing.T) {
	got := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "private", Text: "/codex fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if got.Kind != KindMigrationHint {
		t.Fatalf("unexpected intent: %+v", got)
	}
}

func TestGroupRequiresBotMentionAndProject(t *testing.T) {
	plain := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "group", Text: "@backend fix tests"},
		ProjectAliases: []string{"backend"},
	})
	if plain.Kind != KindIgnored {
		t.Fatalf("plain group message should be ignored: %+v", plain)
	}
	missingProject := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "group", Text: "fix tests", BotMentioned: true},
		ProjectAliases: []string{"backend"},
	})
	if missingProject.Kind != KindProjectSelection {
		t.Fatalf("group mention without project should select project: %+v", missingProject)
	}
	start := ParseStart(ParseInput{
		Event:          contracts.InboundEvent{ChatType: "group", Text: "@backend fix tests", BotMentioned: true},
		ProjectAliases: []string{"backend"},
	})
	if start.Kind != KindStartTask || start.ProjectAlias != "backend" || start.Prompt != "fix tests" {
		t.Fatalf("unexpected start intent: %+v", start)
	}
}
