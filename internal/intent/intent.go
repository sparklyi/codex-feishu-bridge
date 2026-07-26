package intent

type Kind string

const (
	KindIgnored         Kind = "ignored"
	KindStartTask       Kind = "start_task"
	KindUnknownProject  Kind = "unknown_project"
	KindThreadSelection Kind = "thread_selection"
	KindRestartService  Kind = "restart_service"
)

type ParseInput struct {
	Text           string
	ProjectAliases []string
}

type Intent struct {
	Kind         Kind
	Prompt       string
	ProjectAlias string
}
