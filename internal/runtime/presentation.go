package runtime

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

// itemEventParams intentionally accepts only the portable portion of the
// app-server item envelope. Item payloads gain fields over time, so the reducer
// uses just type, command classification, exit status, and file counts.
type itemEventParams struct {
	ThreadID string        `json:"threadId"`
	TurnID   string        `json:"turnId"`
	Item     appServerItem `json:"item"`
}

type appServerItem struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Command  string          `json:"command"`
	Query    string          `json:"query"`
	Status   string          `json:"status"`
	ExitCode *int            `json:"exitCode"`
	Changes  json.RawMessage `json:"changes"`
	Files    json.RawMessage `json:"files"`
}

type displayItemUpdate struct {
	stage         string
	activity      string
	milestone     string
	milestoneKind string
	change        string
	verification  string
}

func classifyDisplayItem(item appServerItem, completed bool) (displayItemUpdate, bool) {
	switch item.Type {
	case "commandExecution":
		return classifyCommandExecution(item, completed), true
	case "fileChange":
		return classifyFileChange(item, completed), true
	case "webSearch":
		if completed {
			return displayItemUpdate{stage: "查阅资料", activity: "已完成参考资料查阅。", milestone: "已查阅参考资料", milestoneKind: "research"}, true
		}
		return displayItemUpdate{stage: "查阅资料", activity: "正在查阅文档和参考资料。"}, true
	case "agentMessage":
		if completed {
			return displayItemUpdate{}, false
		}
		return displayItemUpdate{stage: "整理结果", activity: "正在整理最终回复。"}, true
	default:
		// Reasoning and unknown item kinds are deliberately not surfaced. The
		// task card tracks meaningful work, not the agent's internal trace.
		return displayItemUpdate{}, false
	}
}

func classifyCommandExecution(item appServerItem, completed bool) displayItemUpdate {
	stage, running, done, kind := commandDisplayCopy(item.Command)
	if !completed {
		return displayItemUpdate{stage: stage, activity: running}
	}
	if itemFailed(item) {
		return displayItemUpdate{
			stage:         stage,
			activity:      done + "，发现需要处理的问题。",
			milestone:     done + "，发现问题",
			milestoneKind: kind,
			verification:  verificationFor(kind, false),
		}
	}
	return displayItemUpdate{
		stage:         stage,
		activity:      done + "。",
		milestone:     done,
		milestoneKind: kind,
		verification:  verificationFor(kind, true),
	}
}

func itemFailed(item appServerItem) bool {
	if item.ExitCode != nil {
		return *item.ExitCode != 0
	}
	return item.Status == "failed" || item.Status == "declined"
}

func commandDisplayCopy(command string) (stage, running, done, kind string) {
	value := strings.ToLower(command)
	switch {
	case containsAny(value, "go test", "npm test", "pnpm test", "yarn test", "pytest", "cargo test", "make test", "vitest", "jest", "rspec"):
		return "验证", "正在执行测试。", "已完成测试", "test"
	case containsAny(value, "go vet", "golangci", "eslint", "stylelint", "ruff", "staticcheck", "clippy", "lint"):
		return "检查", "正在执行静态检查。", "已完成静态检查", "check"
	case containsAny(value, "go build", "npm run build", "pnpm build", "yarn build", "cargo build", "make build", "webpack", "vite build"):
		return "构建", "正在构建项目。", "已完成构建", "build"
	case containsAny(value, "rg ", "ripgrep", "grep ", "sed ", "cat ", "head ", "tail ", "find ", "git diff", "git status", "git log"), isListCommand(value):
		return "分析代码", "正在读取代码和变更。", "已完成代码分析", "read"
	default:
		return "执行任务", "正在执行开发操作。", "已完成当前操作", "work"
	}
}

func isListCommand(command string) bool {
	command = strings.TrimSpace(command)
	return command == "ls" || strings.HasPrefix(command, "ls ")
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func verificationFor(kind string, passed bool) string {
	if kind != "test" && kind != "check" && kind != "build" {
		return ""
	}
	label := map[string]string{"test": "测试", "check": "静态检查", "build": "构建"}[kind]
	if passed {
		return label + "已完成"
	}
	return label + "发现问题"
}

func classifyFileChange(item appServerItem, completed bool) displayItemUpdate {
	if !completed {
		return displayItemUpdate{stage: "修改文件", activity: "正在修改文件。"}
	}
	count := itemFileCount(item)
	label := "已修改文件"
	change := "已修改文件"
	if count > 0 {
		label = "已修改 " + itoa(count) + " 个文件"
		change = "修改 " + itoa(count) + " 个文件"
	}
	return displayItemUpdate{
		stage:         "修改文件",
		activity:      label + "。",
		milestone:     label,
		milestoneKind: "change",
		change:        change,
	}
}

func itemFileCount(item appServerItem) int {
	for _, raw := range []json.RawMessage{item.Changes, item.Files} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) == nil && len(values) > 0 {
			return len(values)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && len(object) > 0 {
			return len(object)
		}
		return 1
	}
	return 0
}

func (a *activeRun) applyDisplayItem(item appServerItem, completed bool) bool {
	update, ok := classifyDisplayItem(item, completed)
	if !ok {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal {
		return false
	}
	changed := false
	if update.stage != "" && update.stage != a.stage {
		a.stage = update.stage
		changed = true
	}
	if update.activity != "" && update.activity != a.activity {
		a.activity = update.activity
		changed = true
	}
	if completed && update.milestone != "" {
		a.milestones, changed = appendTaskMilestone(a.milestones, contracts.TaskMilestone{Label: update.milestone, Kind: update.milestoneKind}, changed)
	}
	if completed && update.change != "" {
		a.changes = appendBoundedUnique(a.changes, update.change, 5)
	}
	if completed && update.verification != "" {
		a.verification = appendBoundedUnique(a.verification, update.verification, 5)
	}
	return changed
}

func appendTaskMilestone(values []contracts.TaskMilestone, value contracts.TaskMilestone, changed bool) ([]contracts.TaskMilestone, bool) {
	for _, existing := range values {
		if existing.Label == value.Label {
			return values, changed
		}
	}
	values = append(values, value)
	if len(values) > 5 {
		values = values[len(values)-5:]
	}
	return values, true
}

func appendBoundedUnique(values []string, value string, max int) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	values = append(values, value)
	if len(values) > max {
		values = values[len(values)-max:]
	}
	return values
}

func (a *activeRun) setActivity(stage, activity string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal || (a.stage == stage && a.activity == activity) {
		return false
	}
	a.stage = stage
	a.activity = activity
	return true
}

func (a *activeRun) appendDraft(delta string) bool {
	if strings.TrimSpace(delta) == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal {
		return false
	}
	a.draftText = trimDraftText(a.draftText + delta)
	if a.displayMode != "preview" {
		return false
	}
	current := strings.TrimSpace(a.draftText)
	if !substantiveDraftDelta(a.lastDraftPreview, current) {
		return false
	}
	a.lastDraftPreview = current
	return true
}

func trimDraftText(text string) string {
	const limit = 1200
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return "..." + string(runes[len(runes)-limit:])
}

func substantiveDraftDelta(previous, current string) bool {
	if current == "" || current == previous {
		return false
	}
	delta := current
	if strings.HasPrefix(current, previous) {
		delta = strings.TrimPrefix(current, previous)
	}
	if utf8.RuneCountInString(strings.TrimSpace(delta)) >= 48 {
		return true
	}
	return strings.ContainsAny(delta, "。！？.!?\n")
}

func (a *activeRun) progressPresentation() contracts.TaskPresentation {
	a.mu.Lock()
	defer a.mu.Unlock()
	presentation := contracts.TaskPresentation{
		Layout:     contracts.TaskCardRunning,
		Stage:      a.stage,
		Activity:   a.activity,
		Milestones: append([]contracts.TaskMilestone(nil), a.milestones...),
	}
	if a.displayMode == "preview" {
		presentation.Draft = strings.TrimSpace(a.draftText)
	}
	return presentation
}

func (a *activeRun) resultPresentation(status, fallback string) contracts.TaskPresentation {
	a.mu.Lock()
	finalText := strings.TrimSpace(a.finalText)
	if finalText == "" {
		finalText = strings.TrimSpace(fallback)
	}
	changes := append([]string(nil), a.changes...)
	verification := append([]string(nil), a.verification...)
	a.mu.Unlock()

	conclusion, responseChanges, responseVerification := summarizeFinalText(finalText)
	for _, value := range responseChanges {
		changes = appendBoundedUnique(changes, value, 5)
	}
	for _, value := range responseVerification {
		verification = appendBoundedUnique(verification, value, 5)
	}
	if conclusion == "" {
		conclusion = finalText
	}
	if conclusion == "" {
		conclusion = terminalConclusion(status)
	}
	return contracts.TaskPresentation{
		Layout:       contracts.TaskCardResult,
		Conclusion:   conclusion,
		Changes:      changes,
		Verification: verification,
	}
}

func terminalConclusion(status string) string {
	switch status {
	case "canceled":
		return "任务已停止。"
	case "failed":
		return "任务未完成。"
	default:
		return "任务已完成。"
	}
}

func summarizeFinalText(text string) (string, []string, []string) {
	var conclusion string
	changes := make([]string, 0, 3)
	verification := make([]string, 0, 3)
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if next := finalSection(line); next != "" {
			section = next
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "-*0123456789. "))
		if line == "" {
			continue
		}
		switch section {
		case "conclusion":
			if conclusion == "" {
				conclusion = shortDisplayText(line, 420)
			}
		case "change":
			changes = appendBoundedUnique(changes, shortDisplayText(line, 220), 5)
		case "verification":
			verification = appendBoundedUnique(verification, shortDisplayText(line, 220), 5)
		default:
			if conclusion == "" {
				conclusion = shortDisplayText(line, 420)
			}
		}
	}
	return conclusion, changes, verification
}

func finalSection(line string) string {
	raw := strings.TrimSpace(line)
	value := strings.ToLower(strings.TrimSpace(strings.Trim(raw, "#*：: ")))
	if !sectionHeading(raw, value) {
		return ""
	}
	switch {
	case containsAny(value, "结论", "总结", "结果", "summary", "result"):
		return "conclusion"
	case containsAny(value, "改动", "修改", "changes", "changed", "implementation"):
		return "change"
	case containsAny(value, "验证", "测试", "检查", "verification", "tests", "test", "checks"):
		return "verification"
	default:
		return ""
	}
}

func sectionHeading(raw, value string) bool {
	if strings.HasPrefix(raw, "#") || (strings.HasPrefix(raw, "**") && strings.HasSuffix(raw, "**")) || strings.HasSuffix(raw, ":") || strings.HasSuffix(raw, "：") {
		return true
	}
	switch value {
	case "结论", "总结", "结果", "summary", "result", "改动", "修改", "changes", "changed", "implementation", "验证", "测试", "检查", "verification", "tests", "test", "checks":
		return true
	default:
		return false
	}
}

func shortDisplayText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "..."
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
