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
	CardDetails         CardKind = "details"
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
	TaskCardDetails TaskCardLayout = "details"
)

// TaskMilestone is a bounded, user-facing event summary. It intentionally
// carries no command output, reasoning, or transport detail.
type TaskMilestone struct {
	Label string
	Kind  string
}

// TaskPresentation contains the structured information needed by the three
// task-card layouts. The final response itself remains available only through
// the paged details layout when it does not fit in the result summary.
type TaskPresentation struct {
	Layout       TaskCardLayout
	Stage        string
	Activity     string
	Milestones   []TaskMilestone
	Draft        string
	Conclusion   string
	Changes      []string
	Verification []string
	DetailText   string
	DetailPage   int
	DetailPages  int
}

type OutboundMessage struct {
	ChatID           string
	ReplyToMessageID string
	UpdateMessageID  string
	CardKind         CardKind
	TaskID           string
	Status           string
	Title            string
	Subtitle         string
	BodyMarkdown     string
	Presentation     *TaskPresentation
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
