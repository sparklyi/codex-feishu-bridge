package appserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSchemaDirectoryAcceptsBridgeContracts(t *testing.T) {
	dir := t.TempDir()
	writeCompatibleSchemas(t, dir)
	if err := ValidateSchemaDirectory(dir); err != nil {
		t.Fatalf("validate compatible schemas: %v", err)
	}
}

func TestValidateSchemaDirectoryRejectsMissingCriticalField(t *testing.T) {
	dir := t.TempDir()
	writeCompatibleSchemas(t, dir)
	writeSchema(t, dir, "v2/TurnStartParams.json", paramsSchema(
		[]string{"threadId", "input"},
		[]string{"threadId", "input", "cwd", "model", "approvalPolicy"},
		map[string]any{
			"SandboxPolicy":  map[string]any{"enum": []string{"dangerFullAccess"}},
			"AskForApproval": map[string]any{"enum": []string{approvalPolicy}},
		},
	))
	err := ValidateSchemaDirectory(dir)
	if err == nil || !strings.Contains(err.Error(), "sandboxPolicy") {
		t.Fatalf("schema error = %v, want missing sandboxPolicy", err)
	}
}

func writeCompatibleSchemas(t *testing.T, dir string) {
	t.Helper()
	writeSchema(t, dir, "v1/InitializeParams.json", map[string]any{
		"required": []string{"clientInfo"},
		"properties": map[string]any{
			"clientInfo":   map[string]any{},
			"capabilities": map[string]any{},
		},
		"definitions": map[string]any{
			"ClientInfo":             map[string]any{"required": []string{"name", "version"}},
			"InitializeCapabilities": map[string]any{"properties": map[string]any{"experimentalApi": map[string]any{}}},
		},
	})

	threadDefinitions := map[string]any{
		"SandboxMode":    map[string]any{"enum": []string{fullAccessSandbox}},
		"AskForApproval": map[string]any{"enum": []string{approvalPolicy}},
	}
	writeSchema(t, dir, "v2/ThreadListParams.json", paramsSchema(nil, []string{"limit", "sortKey", "sortDirection"}, nil))
	writeSchema(t, dir, "v2/ThreadStartParams.json", paramsSchema(nil, []string{"cwd", "model", "sandbox", "approvalPolicy"}, threadDefinitions))
	writeSchema(t, dir, "v2/ThreadResumeParams.json", paramsSchema([]string{"threadId"}, []string{"threadId", "cwd", "model", "sandbox", "approvalPolicy"}, threadDefinitions))
	writeSchema(t, dir, "v2/TurnStartParams.json", paramsSchema(
		[]string{"threadId", "input"},
		[]string{"threadId", "input", "cwd", "model", "sandboxPolicy", "approvalPolicy"},
		map[string]any{
			"SandboxPolicy":  map[string]any{"enum": []string{"dangerFullAccess"}},
			"AskForApproval": map[string]any{"enum": []string{approvalPolicy}},
		},
	))
	writeSchema(t, dir, "v2/TurnSteerParams.json", paramsSchema([]string{"threadId", "expectedTurnId", "input"}, []string{"threadId", "expectedTurnId", "input"}, nil))
	writeSchema(t, dir, "v2/TurnInterruptParams.json", paramsSchema([]string{"threadId", "turnId"}, []string{"threadId", "turnId"}, nil))
}

func paramsSchema(required, properties []string, definitions map[string]any) map[string]any {
	fields := make(map[string]any, len(properties))
	for _, property := range properties {
		fields[property] = map[string]any{}
	}
	return map[string]any{
		"required":    required,
		"properties":  fields,
		"definitions": definitions,
	}
}

func writeSchema(t *testing.T, dir, relative string, document map[string]any) {
	t.Helper()
	path := filepath.Join(dir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
