package transport

import (
	"errors"
	"testing"
)

func TestIsTransientErrorRecognizesSDKFormattedTimeouts(t *testing.T) {
	cases := []string{
		`Patch "https://open.feishu.cn/open-apis/im/v1/messages/card": context deadline exceeded`,
		`Post "https://open.feishu.cn/open-apis/im/v1/messages": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
		`Post "https://open.feishu.cn/open-apis/im/v1/messages": read: i/o timeout`,
		`Patch "https://open.feishu.cn/open-apis/im/v1/messages/card": read: operation timed out`,
	}

	for _, message := range cases {
		t.Run(message, func(t *testing.T) {
			if !IsTransientError(errors.New(message)) {
				t.Fatalf("IsTransientError(%q) = false, want true", message)
			}
		})
	}
}

func TestIsTransientErrorRejectsPermanentError(t *testing.T) {
	if IsTransientError(errors.New("Feishu patch failed: code=230001 msg=invalid message id")) {
		t.Fatal("permanent API error classified as transient")
	}
}
