package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorCommandRendersReport(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(`
[feishu]
app_secret_env = "FEISHU_APP_SECRET"

[workspace]
default = "/missing"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runWithIO(context.Background(), []string{"doctor", "--config", configPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL feishu.app_id") {
		t.Fatalf("expected doctor report, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
