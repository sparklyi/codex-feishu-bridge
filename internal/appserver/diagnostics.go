package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrSchemaGeneratorUnavailable means this Codex CLI does not expose the
// app-server schema generator. A successful protocol probe can still be used
// with older CLI releases, so callers should surface this as a warning.
var ErrSchemaGeneratorUnavailable = errors.New("app-server schema generator unavailable")

const schemaCommandOutputLimit = 2 * 1024

// Version returns the standalone Codex CLI version reported by --version.
func Version(ctx context.Context, command string) (string, error) {
	command = defaultCommand(command)
	output, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
	version := strings.TrimSpace(string(output))
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", command, commandError(err, version))
	}
	if version == "" {
		return "", fmt.Errorf("run %s --version: empty output", command)
	}
	return version, nil
}

// CheckSchema generates the CLI's published app-server schemas and verifies
// only the stable request contracts used by this bridge.
func CheckSchema(ctx context.Context, command string) error {
	command = defaultCommand(command)
	dir, err := os.MkdirTemp("", "codex-app-server-schema-")
	if err != nil {
		return fmt.Errorf("create app-server schema directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	output, err := exec.CommandContext(ctx, command, "app-server", "generate-json-schema", "--out", dir).CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("generate app-server schemas: %w", ctx.Err())
		}
		commandErr := commandError(err, strings.TrimSpace(string(output)))
		if schemaGeneratorUnavailable(commandErr.Error()) {
			return fmt.Errorf("%w: %s", ErrSchemaGeneratorUnavailable, commandErr)
		}
		return fmt.Errorf("generate app-server schemas: %w", commandErr)
	}
	if err := ValidateSchemaDirectory(dir); err != nil {
		return err
	}
	return nil
}

// ValidateSchemaDirectory checks a generated schema directory. It is exported
// for deterministic diagnostics tests and contract workflows.
func ValidateSchemaDirectory(dir string) error {
	var issues []string
	initialize := loadSchema(filepath.Join(dir, "v1", "InitializeParams.json"), &issues)
	checkRequired(&issues, "v1/InitializeParams", initialize, "clientInfo")
	checkProperties(&issues, "v1/InitializeParams", initialize, "clientInfo", "capabilities")
	checkDefinitionRequired(&issues, "v1/InitializeParams", initialize, "ClientInfo", "name", "version")
	checkDefinitionProperties(&issues, "v1/InitializeParams", initialize, "InitializeCapabilities", "experimentalApi")

	threadList := loadSchema(filepath.Join(dir, "v2", "ThreadListParams.json"), &issues)
	checkProperties(&issues, "v2/ThreadListParams", threadList, "limit", "sortKey", "sortDirection")

	threadStart := loadSchema(filepath.Join(dir, "v2", "ThreadStartParams.json"), &issues)
	checkProperties(&issues, "v2/ThreadStartParams", threadStart, "cwd", "model", "sandbox", "approvalPolicy")
	checkDefinitionEnum(&issues, "v2/ThreadStartParams", threadStart, "SandboxMode", fullAccessSandbox)
	checkDefinitionEnum(&issues, "v2/ThreadStartParams", threadStart, "AskForApproval", approvalPolicy)

	threadResume := loadSchema(filepath.Join(dir, "v2", "ThreadResumeParams.json"), &issues)
	checkRequired(&issues, "v2/ThreadResumeParams", threadResume, "threadId")
	checkProperties(&issues, "v2/ThreadResumeParams", threadResume, "threadId", "cwd", "model", "sandbox", "approvalPolicy")
	checkDefinitionEnum(&issues, "v2/ThreadResumeParams", threadResume, "SandboxMode", fullAccessSandbox)
	checkDefinitionEnum(&issues, "v2/ThreadResumeParams", threadResume, "AskForApproval", approvalPolicy)

	turnStart := loadSchema(filepath.Join(dir, "v2", "TurnStartParams.json"), &issues)
	checkRequired(&issues, "v2/TurnStartParams", turnStart, "threadId", "input")
	checkProperties(&issues, "v2/TurnStartParams", turnStart, "threadId", "input", "cwd", "model", "sandboxPolicy", "approvalPolicy")
	checkDefinitionEnum(&issues, "v2/TurnStartParams", turnStart, "SandboxPolicy", "dangerFullAccess")
	checkDefinitionEnum(&issues, "v2/TurnStartParams", turnStart, "AskForApproval", approvalPolicy)

	turnSteer := loadSchema(filepath.Join(dir, "v2", "TurnSteerParams.json"), &issues)
	checkRequired(&issues, "v2/TurnSteerParams", turnSteer, "threadId", "expectedTurnId", "input")
	checkProperties(&issues, "v2/TurnSteerParams", turnSteer, "threadId", "expectedTurnId", "input")

	turnInterrupt := loadSchema(filepath.Join(dir, "v2", "TurnInterruptParams.json"), &issues)
	checkRequired(&issues, "v2/TurnInterruptParams", turnInterrupt, "threadId", "turnId")
	checkProperties(&issues, "v2/TurnInterruptParams", turnInterrupt, "threadId", "turnId")

	if len(issues) > 0 {
		return fmt.Errorf("app-server schema is incompatible: %s", strings.Join(issues, "; "))
	}
	return nil
}

type schemaDocument struct {
	Required    []string                   `json:"required"`
	Properties  map[string]json.RawMessage `json:"properties"`
	Definitions map[string]json.RawMessage `json:"definitions"`
}

func defaultCommand(command string) string {
	if command == "" {
		return "codex"
	}
	return command
}

func commandError(err error, output string) error {
	if len(output) > schemaCommandOutputLimit {
		output = output[:schemaCommandOutputLimit] + "..."
	}
	if output == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, output)
}

func schemaGeneratorUnavailable(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "generate-json-schema") &&
		(strings.Contains(message, "unknown") || strings.Contains(message, "unrecognized") || strings.Contains(message, "unsupported"))
}

func loadSchema(path string, issues *[]string) schemaDocument {
	data, err := os.ReadFile(path)
	if err != nil {
		*issues = append(*issues, schemaPathLabel(path)+": cannot read schema")
		return schemaDocument{}
	}
	var document schemaDocument
	if err := json.Unmarshal(data, &document); err != nil {
		*issues = append(*issues, schemaPathLabel(path)+": "+err.Error())
	}
	return document
}

func schemaPathLabel(path string) string {
	return filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(path)), filepath.Base(path)))
}

func checkRequired(issues *[]string, name string, document schemaDocument, required ...string) {
	for _, property := range required {
		if !contains(document.Required, property) {
			*issues = append(*issues, name+" must require "+property)
		}
	}
}

func checkProperties(issues *[]string, name string, document schemaDocument, properties ...string) {
	for _, property := range properties {
		if _, ok := document.Properties[property]; !ok {
			*issues = append(*issues, name+" must support "+property)
		}
	}
}

func checkDefinitionRequired(issues *[]string, name string, document schemaDocument, definition string, required ...string) {
	node, ok := decodeDefinition(document, definition)
	if !ok {
		*issues = append(*issues, name+" is missing definition "+definition)
		return
	}
	checkRequired(issues, name+"/"+definition, node, required...)
}

func checkDefinitionProperties(issues *[]string, name string, document schemaDocument, definition string, properties ...string) {
	node, ok := decodeDefinition(document, definition)
	if !ok {
		*issues = append(*issues, name+" is missing definition "+definition)
		return
	}
	checkProperties(issues, name+"/"+definition, node, properties...)
}

func checkDefinitionEnum(issues *[]string, name string, document schemaDocument, definition, expected string) {
	raw, ok := document.Definitions[definition]
	if !ok {
		*issues = append(*issues, name+" is missing definition "+definition)
		return
	}
	if !enumContains(raw, expected) {
		*issues = append(*issues, name+" must allow "+expected)
	}
}

func decodeDefinition(document schemaDocument, name string) (schemaDocument, bool) {
	raw, ok := document.Definitions[name]
	if !ok {
		return schemaDocument{}, false
	}
	var node schemaDocument
	if err := json.Unmarshal(raw, &node); err != nil {
		return schemaDocument{}, false
	}
	return node, true
}

func enumContains(raw json.RawMessage, expected string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return enumValueContains(value, expected)
}

func enumValueContains(value any, expected string) bool {
	switch node := value.(type) {
	case map[string]any:
		if values, ok := node["enum"].([]any); ok {
			for _, value := range values {
				if stringValue, ok := value.(string); ok && stringValue == expected {
					return true
				}
			}
		}
		for _, value := range node {
			if enumValueContains(value, expected) {
				return true
			}
		}
	case []any:
		for _, value := range node {
			if enumValueContains(value, expected) {
				return true
			}
		}
	}
	return false
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
