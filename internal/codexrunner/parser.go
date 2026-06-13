package codexrunner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type ParsedOutput struct {
	CodexSessionID string
	FinalText      string
}

type ParserCallbacks struct {
	OnSessionID func(threadID string) error
}

func ParseJSONL(r io.Reader) (ParsedOutput, error) {
	return ParseJSONLStream(r, ParserCallbacks{})
}

func ParseJSONLStream(r io.Reader, callbacks ParserCallbacks) (ParsedOutput, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var out ParsedOutput
	sessionCallbackCalled := false
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return ParsedOutput{}, fmt.Errorf("malformed JSONL at line %d: %w", lineNo, err)
		}
		sessionID := event.sessionID()
		if sessionID != "" && out.CodexSessionID == "" {
			out.CodexSessionID = sessionID
			if callbacks.OnSessionID != nil && !sessionCallbackCalled {
				sessionCallbackCalled = true
				if err := callbacks.OnSessionID(sessionID); err != nil {
					return ParsedOutput{}, err
				}
			}
		}
		if event.Type == "item.completed" && event.Item.Type == "agent_message" {
			out.FinalText = event.Item.Text
		}
	}
	if err := scanner.Err(); err != nil {
		return ParsedOutput{}, err
	}
	if out.CodexSessionID == "" {
		return ParsedOutput{}, fmt.Errorf("missing session id")
	}
	if out.FinalText == "" {
		return ParsedOutput{}, fmt.Errorf("missing final assistant text")
	}
	return out, nil
}

type codexEvent struct {
	Type      string    `json:"type"`
	ThreadID  string    `json:"thread_id"`
	SessionID string    `json:"session_id"`
	Item      codexItem `json:"item"`
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (e codexEvent) sessionID() string {
	if e.Type == "thread.started" && e.ThreadID != "" {
		return e.ThreadID
	}
	return e.SessionID
}
