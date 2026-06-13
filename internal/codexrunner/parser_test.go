package codexrunner

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseJSONLSuccessFixture(t *testing.T) {
	file, err := os.Open("testdata/success.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := ParseJSONL(file)
	if err != nil {
		t.Fatal(err)
	}
	if got.CodexSessionID != "019ec257-e6fd-7be1-9a5e-c47442df292c" || got.FinalText != "OK" {
		t.Fatalf("unexpected parsed output: %+v", got)
	}
}

func TestParseJSONLStreamCallsSessionCallbackImmediatelyOnce(t *testing.T) {
	input := strings.NewReader(`{"type":"thread.started","thread_id":"thread_1"}
{"type":"item.completed","item":{"type":"agent_message","text":"first"}}
{"type":"thread.started","thread_id":"thread_2"}
{"type":"item.completed","item":{"type":"agent_message","text":"last"}}
`)
	var calls []string
	got, err := ParseJSONLStream(input, ParserCallbacks{OnSessionID: func(threadID string) error {
		calls = append(calls, threadID)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "thread_1" {
		t.Fatalf("unexpected session callbacks: %+v", calls)
	}
	if got.FinalText != "last" {
		t.Fatalf("expected last agent message, got %+v", got)
	}
}

func TestParseMalformedJSONL(t *testing.T) {
	file, err := os.Open("testdata/malformed.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := ParseJSONL(file); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed error, got %v", err)
	}
}

func TestParseMissingSessionID(t *testing.T) {
	input := strings.NewReader(`{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}`)
	if _, err := ParseJSONL(input); err == nil || !strings.Contains(err.Error(), "missing session id") {
		t.Fatalf("expected missing session id, got %v", err)
	}
}

func TestParseMissingFinalText(t *testing.T) {
	input := strings.NewReader(`{"type":"thread.started","thread_id":"thread_1"}`)
	if _, err := ParseJSONL(input); err == nil || !strings.Contains(err.Error(), "missing final assistant text") {
		t.Fatalf("expected missing final text, got %v", err)
	}
}

func TestParseSessionIDFallback(t *testing.T) {
	input := strings.NewReader(`{"session_id":"fallback_1"}
{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}`)
	got, err := ParseJSONL(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.CodexSessionID != "fallback_1" {
		t.Fatalf("unexpected fallback session id: %+v", got)
	}
}

func TestParseSessionCallbackErrorStopsParsing(t *testing.T) {
	want := errors.New("store failed")
	input := strings.NewReader(`{"type":"thread.started","thread_id":"thread_1"}
{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}`)
	_, err := ParseJSONLStream(input, ParserCallbacks{OnSessionID: func(string) error { return want }})
	if !errors.Is(err, want) {
		t.Fatalf("expected callback error, got %v", err)
	}
}
