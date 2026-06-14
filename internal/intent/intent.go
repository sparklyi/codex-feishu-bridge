package intent

import "github.com/sparklyi/codex-feishu-bridge/internal/contracts"

type Kind string

const (
	KindIgnored          Kind = "ignored"
	KindStartTask        Kind = "start_task"
	KindProjectSelection Kind = "project_selection"
	KindUnknownProject   Kind = "unknown_project"
	KindMigrationHint    Kind = "migration_hint"
)

type ParseInput struct {
	Event          contracts.InboundEvent
	ProjectAliases []string
}

type Intent struct {
	Kind         Kind
	Prompt       string
	ProjectAlias string
}
