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
	ChatType      string // "private" or "non_private"
	ChatID        string
	SenderOpenID  string
	MessageID     string
	RootMessageID string
	ActionID      string
	ActionValue   map[string]string
	Text          string
	RawReceivedAt time.Time
}

type CardKind string

const (
	CardStart           CardKind = "start"
	CardSuccess         CardKind = "success"
	CardFailure         CardKind = "failure"
	CardRestarting      CardKind = "restarting"
	CardRoutingError    CardKind = "routing_error"
	CardThreadSelection CardKind = "thread_selection"
	CardRunningConflict CardKind = "running_conflict"
)

// TaskCardLayout selects the specialized developer-facing task card layout.
// Other card kinds continue to use the generic card layout.
type TaskCardLayout string

const (
	TaskCardRunning TaskCardLayout = "running"
	TaskCardResult  TaskCardLayout = "result"
)

// TaskMilestone is a bounded, user-facing event summary. It intentionally
// carries no command output, reasoning, or transport detail.
type TaskMilestone struct {
	Label string
	Kind  string
}

// TaskPresentation contains the structured information needed by task cards.
// Result cards expose the bounded final AI response as their conclusion.
type TaskPresentation struct {
	Layout           TaskCardLayout
	Stage            string
	Activity         string
	UserInputs       []string
	Milestones       []TaskMilestone
	ProcessingDetail string
	Conclusion       string
	Changes          []string
	Verification     []string
}

type OutboundMessage struct {
	ChatID           string
	ReplyToMessageID string
	UpdateMessageID  string
	// DeliveryMaxAttempts overrides the sender-level retry policy for this
	// message. Zero preserves the sender default.
	DeliveryMaxAttempts int
	CardKind            CardKind
	TaskID              string
	Status              string
	Title               string
	Subtitle            string
	BodyMarkdown        string
	Presentation        *TaskPresentation
	// StreamDetail marks a running task card whose processing-detail Markdown
	// element can be updated through CardKit without rebuilding the full card.
	StreamDetail bool
	Fields       []Field
	Options      []CardOption
	Actions      []Action
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
