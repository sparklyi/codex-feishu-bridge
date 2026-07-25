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
	defer client.Close()

	go func() {
		reader := bufio.NewScanner(serverIn)
		writer := bufio.NewWriter(serverOut)
		defer writer.Flush()
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
	defer client.Close()

	var once sync.Once
	go func() {
		reader := bufio.NewScanner(serverIn)
		writer := bufio.NewWriter(serverOut)
		defer writer.Flush()
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

func TestFullAccessPolicyIsFixed(t *testing.T) {
	if fullAccessSandbox != "danger-full-access" || approvalPolicy != "never" {
		t.Fatalf("unexpected fixed policies sandbox=%q approval=%q", fullAccessSandbox, approvalPolicy)
	}
	if policy := fullAccessSandboxPolicy(); policy["type"] != "dangerFullAccess" || len(policy) != 1 {
		t.Fatalf("unexpected policy: %#v", policy)
	}
}
