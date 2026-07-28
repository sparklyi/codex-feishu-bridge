package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientInitializesAndListsThreads(t *testing.T) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	client := NewClient(clientIn, clientOut, func() error { return nil })
	defer func() { _ = client.Close() }()

	go func() {
		reader := bufio.NewScanner(serverIn)
		writer := bufio.NewWriter(serverOut)
		defer func() { _ = writer.Flush() }()
		for reader.Scan() {
			var message map[string]json.RawMessage
			if err := json.Unmarshal(reader.Bytes(), &message); err != nil {
				return
			}
			var method string
			_ = json.Unmarshal(message["method"], &method)
			if method == "initialize" {
				_, _ = writer.WriteString(`{"id":1,"result":{"platformFamily":"macos"}}` + "\n")
				_ = writer.Flush()
			}
			if method == "thread/list" {
				_, _ = writer.WriteString(`{"id":2,"result":{"data":[{"id":"thread-1","cwd":"/repo","preview":"hello","status":{"type":"idle"}}]}}` + "\n")
				_ = writer.Flush()
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Initialize(ctx, "test", "1.0"); err != nil {
		t.Fatal(err)
	}
	threads, err := client.ListThreads(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != "thread-1" || threads[0].CWD != "/repo" {
		t.Fatalf("unexpected threads: %+v", threads)
	}
}

func TestClientRoutesServerRequestAndResponse(t *testing.T) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	client := NewClient(clientIn, clientOut, func() error { return nil })
	defer func() { _ = client.Close() }()

	var once sync.Once
	go func() {
		reader := bufio.NewScanner(serverIn)
		writer := bufio.NewWriter(serverOut)
		defer func() { _ = writer.Flush() }()
		for reader.Scan() {
			line := reader.Text()
			if strings.Contains(line, `"method":"initialized"`) {
				once.Do(func() {
					_, _ = writer.WriteString(`{"id":"approval-1","method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1"}}` + "\n")
					_ = writer.Flush()
				})
				continue
			}
			if strings.Contains(line, `"id":"approval-1"`) {
				return
			}
			if strings.Contains(line, `"method":"initialize"`) {
				_, _ = writer.WriteString(`{"id":1,"result":{}}` + "\n")
				_ = writer.Flush()
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Initialize(ctx, "test", "1.0"); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-client.Requests():
		if request.Method != "item/commandExecution/requestApproval" || request.IDString() != `"approval-1"` {
			t.Fatalf("unexpected request: %+v", request)
		}
		if err := client.Respond(ctx, request.ID, map[string]string{"decision": "accept"}); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for server request")
	}
}

func TestClientInitializeUsesStableProtocolByDefault(t *testing.T) {
	params := captureInitializeParams(t, false)
	if _, ok := params["capabilities"]; ok {
		t.Fatalf("stable initialization unexpectedly opted into capabilities: %+v", params)
	}
}

func TestClientInitializeCanOptIntoExperimentalAPI(t *testing.T) {
	params := captureInitializeParams(t, true)
	capabilities, ok := params["capabilities"].(map[string]any)
	if !ok || capabilities["experimentalApi"] != true {
		t.Fatalf("experimental initialization capabilities = %#v", params["capabilities"])
	}
}

func captureInitializeParams(t *testing.T, experimentalAPI bool) map[string]any {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	client := NewClient(clientIn, clientOut, func() error { return nil })
	t.Cleanup(func() { _ = client.Close() })

	params := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewScanner(serverIn)
		writer := bufio.NewWriter(serverOut)
		defer func() { _ = writer.Flush() }()
		for reader.Scan() {
			var message map[string]json.RawMessage
			if json.Unmarshal(reader.Bytes(), &message) != nil {
				return
			}
			var method string
			_ = json.Unmarshal(message["method"], &method)
			switch method {
			case "initialize":
				var got map[string]any
				_ = json.Unmarshal(message["params"], &got)
				params <- got
				_, _ = writer.WriteString(`{"id":1,"result":{}}` + "\n")
				_ = writer.Flush()
			case "initialized":
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var err error
	if experimentalAPI {
		err = client.initialize(ctx, "test", "1.0", true)
	} else {
		err = client.Initialize(ctx, "test", "1.0")
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-params:
		return got
	case <-ctx.Done():
		t.Fatal("timed out waiting for initialize request")
		return nil
	}
}

func TestClientCloseStopsReadLoopAndClosesStreams(t *testing.T) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	t.Cleanup(func() {
		_ = serverIn.Close()
		_ = serverOut.Close()
	})
	client := NewClient(clientIn, clientOut, func() error { return nil })

	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close client: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client close did not wait for the reader loop")
	}
	if _, ok := <-client.Events(); ok {
		t.Fatal("events stream remains open after client close")
	}
	if _, ok := <-client.Requests(); ok {
		t.Fatal("requests stream remains open after client close")
	}
}

func TestClientWriteFailureShutsDownReadLoop(t *testing.T) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	if err := serverIn.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverOut.Close() })
	client := NewClient(clientIn, clientOut, func() error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Call(ctx, "thread/list", map[string]any{}, nil); err == nil {
		t.Fatal("call succeeded after the app-server input was closed")
	}
	select {
	case <-client.readDone:
	case <-time.After(time.Second):
		t.Fatal("write failure did not stop the reader loop")
	}
}

func TestFullAccessPolicyIsFixed(t *testing.T) {
	if fullAccessSandbox != "danger-full-access" || approvalPolicy != "never" {
		t.Fatalf("unexpected fixed policies sandbox=%q approval=%q", fullAccessSandbox, approvalPolicy)
	}
	if policy := fullAccessSandboxPolicy(); policy["type"] != "dangerFullAccess" || len(policy) != 1 {
		t.Fatalf("unexpected policy: %#v", policy)
	}
}

func TestClientSteersCurrentTurnWithoutStartingAnother(t *testing.T) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	client := NewClient(clientIn, clientOut, func() error { return nil })
	defer func() { _ = client.Close() }()

	request := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewScanner(serverIn)
		writer := bufio.NewWriter(serverOut)
		defer func() { _ = writer.Flush() }()
		if !reader.Scan() {
			return
		}
		var message map[string]any
		if json.Unmarshal(reader.Bytes(), &message) != nil {
			return
		}
		request <- message
		_, _ = writer.WriteString(`{"id":1,"result":{"turnId":"turn-1"}}` + "\n")
		_ = writer.Flush()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	turnID, err := client.SteerTurn(ctx, TurnSteerInput{ThreadID: "thread-1", ExpectedTurnID: "turn-1", Text: "also verify the tests"})
	if err != nil || turnID != "turn-1" {
		t.Fatalf("steer result turn=%q err=%v", turnID, err)
	}
	message := <-request
	if message["method"] != "turn/steer" {
		t.Fatalf("method = %#v", message["method"])
	}
	params, ok := message["params"].(map[string]any)
	if !ok || params["threadId"] != "thread-1" || params["expectedTurnId"] != "turn-1" {
		t.Fatalf("unexpected steer params: %#v", message["params"])
	}
	input, ok := params["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("steer input malformed: %#v", params["input"])
	}
	part, ok := input[0].(map[string]any)
	if !ok || part["type"] != "text" || part["text"] != "also verify the tests" {
		t.Fatalf("steer text malformed: %#v", input[0])
	}
}
