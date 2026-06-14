package intent

type Shortcut struct {
	Prompt    string
	Immediate bool
}

var ShortcutPrompts = map[string]Shortcut{
	"summarize":      {Prompt: "Summarize the current task result and next steps.", Immediate: true},
	"explain_error":  {Prompt: "Explain the latest error and suggest the next debugging step.", Immediate: true},
	"run_tests":      {Prompt: "Run the relevant tests for the current change and report the result.", Immediate: false},
	"mr_description": {Prompt: "Draft a concise merge request description for the current change.", Immediate: false},
}

func LookupShortcut(name string) (Shortcut, bool) {
	shortcut, ok := ShortcutPrompts[name]
	return shortcut, ok
}
