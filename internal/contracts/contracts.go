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
	ActionValue   map[string]string
	Text          string
	RawReceivedAt time.Time
}

type CardKind string

const (
	CardStart            CardKind = "start"
	CardSuccess          CardKind = "success"
	CardFailure          CardKind = "failure"
	CardRoutingError     CardKind = "routing_error"
	CardProjectSelection CardKind = "project_selection"
	CardThreadSelection  CardKind = "thread_selection"
	CardRunningConflict  CardKind = "running_conflict"
)

type OutboundMessage struct {
	ChatID           string
	ReplyToMessageID string
	UpdateMessageID  string
	CardKind         CardKind
	TaskID           string
	Status           string
	Title            string
	BodyMarkdown     string
	Fields           []Field
	Options          []CardOption
	Actions          []Action
}

type Field struct {
	Title string
	Value string
}

// CardOption is a selectable, structured row in a card.
type CardOption struct {
	Title  string
	Detail string
	Meta   string
	Action Action
}

type Action struct {
	ID    string
	Label string
	Style string
	Value map[string]string
}

type SentMessage struct {
	MessageID string
}
