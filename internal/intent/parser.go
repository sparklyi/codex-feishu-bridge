package intent

import "strings"

func ParseStart(input ParseInput) Intent {
	text := strings.TrimSpace(input.Event.Text)
	if text == "" {
		return Intent{Kind: KindIgnored}
	}
	if strings.HasPrefix(text, "/codex") {
		return Intent{Kind: KindMigrationHint}
	}
	if input.Event.ChatType == "group" && !input.Event.BotMentioned {
		return Intent{Kind: KindIgnored}
	}

	alias, prompt, hasAlias := leadingProjectAlias(text)
	if hasAlias {
		if !hasProjectAlias(input.ProjectAliases, alias) {
			return Intent{Kind: KindUnknownProject, ProjectAlias: alias, Prompt: prompt}
		}
		return Intent{Kind: KindStartTask, ProjectAlias: alias, Prompt: prompt}
	}
	if input.Event.ChatType == "group" {
		return Intent{Kind: KindProjectSelection, Prompt: text}
	}
	return Intent{Kind: KindStartTask, Prompt: text}
}

func leadingProjectAlias(text string) (string, string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") || len(fields[0]) == 1 {
		return "", text, false
	}
	alias := strings.TrimPrefix(fields[0], "@")
	prompt := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	return alias, prompt, true
}

func hasProjectAlias(aliases []string, want string) bool {
	for _, alias := range aliases {
		if alias == want {
			return true
		}
	}
	return false
}
