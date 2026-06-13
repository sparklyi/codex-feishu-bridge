package redact

import (
	"strings"
	"testing"
)

func TestRedactFeishuText(t *testing.T) {
	in := "token=abc123 /Users/sihuo/private /opt/project/file.go http://user:pass@proxy.local:7890"
	got := FeishuText(in, 2000)
	for _, banned := range []string{"abc123", "/Users/sihuo", "/opt/project", "user:pass@"} {
		if strings.Contains(got, banned) {
			t.Fatalf("redaction leaked %q in %q", banned, got)
		}
	}
}

func TestRedactBearerSecretAndKeepsURLPath(t *testing.T) {
	in := "Authorization: Bearer secret-token see https://example.com/path/to/doc"
	got := FeishuText(in, 2000)
	if strings.Contains(got, "secret-token") {
		t.Fatalf("bearer secret leaked: %q", got)
	}
	if !strings.Contains(got, "https://example.com/path/to/doc") {
		t.Fatalf("ordinary URL path should remain visible: %q", got)
	}
}

func TestTruncateAfterRedaction(t *testing.T) {
	got := FeishuText("token=abc123 "+strings.Repeat("x", 100), 20)
	if len(got) > 20 {
		t.Fatalf("expected bounded output, got %d bytes", len(got))
	}
	if strings.Contains(got, "abc123") {
		t.Fatalf("secret leaked after truncation: %q", got)
	}
}
