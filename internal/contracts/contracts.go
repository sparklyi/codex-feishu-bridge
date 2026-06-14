package contracts

import "time"

type InboundKind string

const (
	InboundNewTask    InboundKind = "new_task"
	InboundReply      InboundKind = "reply"
	InboundCardAction InboundKind = "card_action"
)

type InboundEvent struct {
	DedupKey      string
	Kind          InboundKind
	ChatType      string // "private", "group", or "unknown"
	ChatID        string
	SenderOpenID  string
	MessageID     string
	RootMessageID string
	BotMentioned  bool
	ActionID      string
	Text          string
	RawReceivedAt time.Time
}

type CardKind string

const (
	CardStart        CardKind = "start"
	CardSuccess      CardKind = "success"
	CardFailure      CardKind = "failure"
	CardRoutingError CardKind = "routing_error"
)

type OutboundMessage struct {
	ChatID           string
	ReplyToMessageID string
	CardKind         CardKind
	TaskID           string
	Status           string
	Title            string
	BodyMarkdown     string
	Actions          []Action
}

type Action struct {
	ID    string
	Label string
}

type SentMessage struct {
	MessageID string
}

type RunResult struct {
	CodexSessionID string
	FinalText      string
	ExitCode       int
	StderrTail     string
	LogPath        string
	StartedAt      time.Time
	FinishedAt     time.Time
}
