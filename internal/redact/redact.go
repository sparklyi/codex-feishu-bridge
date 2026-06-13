package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	proxyCredentialPattern = regexp.MustCompile(`(?i)(https?://)[^/@\s:]+:[^/@\s]+@`)
	bearerPattern          = regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+[^,\s]+`)
	tokenPattern           = regexp.MustCompile(`(?i)\b(token|app_secret)\s*=\s*[^,\s]+`)
	localPathPattern       = regexp.MustCompile(`(^|[\s\("'])/(?:Users|home|sihuo|private|tmp|var/folders|opt)(?:/[^\s\)"']*)*`)
)

func FeishuText(input string, limit int) string {
	out := proxyCredentialPattern.ReplaceAllString(input, `${1}[redacted]@`)
	out = bearerPattern.ReplaceAllString(out, "Authorization: Bearer [redacted]")
	out = tokenPattern.ReplaceAllString(out, `${1}=[redacted]`)
	out = localPathPattern.ReplaceAllStringFunc(out, func(match string) string {
		if match == "" {
			return match
		}
		first, size := utf8.DecodeRuneInString(match)
		if first == '/' {
			return "[local-path]"
		}
		return match[:size] + "[local-path]"
	})
	return truncate(out, limit)
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
