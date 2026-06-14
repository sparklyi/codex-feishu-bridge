package router

import "strings"

func ParseCommand(text string) (alias string, prompt string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 || fields[0] != "/codex" {
		return "", "", false
	}
	start := 1
	if strings.HasPrefix(fields[1], "@") {
		alias = strings.TrimPrefix(fields[1], "@")
		start = 2
		if alias == "" {
			return "", "", false
		}
	}
	if len(fields) <= start {
		return "", "", false
	}
	return alias, strings.Join(fields[start:], " "), true
}
