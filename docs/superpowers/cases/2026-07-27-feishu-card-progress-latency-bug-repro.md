# Feishu Card Progress Latency Reproduction

```yaml
analysis:
  status: verified
  classification:
    - error-handling
    - concurrency
  expected:
    - A continue action updates the existing task card to a processing state within two seconds.
    - A short non-empty streamed delta is visible within one second on a healthy connection.
  actual_before_fix:
    - A continue action could leave its card unchanged for about ten seconds or longer.
    - A short streamed delta was held until it reached 48 runes or punctuation.
  evidence:
    logs:
      - timestamp: "2026-07-27 01:51:36 to 01:51:51"
        observation: "A card action was received, then the first progress Patch failure appeared about 15 seconds later."
      - timestamp: "2026-07-27 01:52:45 to 01:53:00"
        observation: "The same delay repeated on a second action."
    sqlite:
      - observation: "event_dedup.received_at, runs.started_at, and the task status transition were effectively at the callback time."
  root_cause:
    - "Progress patches inherited the sender's three attempts with a five-second attempt timeout."
    - "The stream reducer ignored deltas shorter than 48 runes without terminal punctuation."
  after_fix:
    - "A progress patch has one sender attempt and a 1500ms runtime deadline; the runtime coalesces and retries the latest state."
    - "Every visible delta enters the existing cadence coalescer."
    - "Transport errors close idle HTTP connections so a failed proxy tunnel is not reused."
coverage_plan:
  trigger_bug:
    - "Use a temporary Patch failure with a sender configured for three attempts."
    - "Use a two-rune agent delta in preview mode."
  non_target_regression:
    - "Terminal task cards retain the sender-level retry policy."
    - "Concise task cards still omit processing detail."
  local_isolation:
    - "Use fake CardAPI and CardNotifier implementations; no Feishu, proxy, or desktop Codex service is required."
cases:
  - id: progress-patch-single-attempt
    target_layer: transport
    minimal_trigger: "A patch with DeliveryMaxAttempts=1 returns a transient error while Sender.MaxAttempts=3."
    before_assertion: "The sender invokes PatchCard more than once and sleeps before retrying."
    after_assertion: "The sender invokes PatchCard exactly once and returns the first transient error."
    obligations:
      - trigger_bug
      - prove_before_fail
      - prove_after_pass
  - id: progress-deadline-and-runtime-retry
    target_layer: controller
    minimal_trigger: "The first progress notification waits for its context to expire, then returns context.DeadlineExceeded."
    before_assertion: "The next notification waits for notification_timeout_seconds rather than the configured progress deadline."
    after_assertion: "The first attempt ends within the short progress deadline and the latest progress is retried within 500ms."
    obligations:
      - trigger_bug
      - prove_before_fail
      - prove_after_pass
      - cover_error_branch
  - id: short-stream-delta
    target_layer: controller
    minimal_trigger: "An active preview task receives the delta ok."
    before_assertion: "No progress card contains the short processing detail."
    after_assertion: "A progress card contains exactly the short processing detail."
    obligations:
      - trigger_bug
      - prove_before_fail
      - prove_after_pass
      - cover_boundary_class
executors:
  - package: internal/transport/feishu
    tests:
      - TestSenderProgressPatchAttemptOverrideStopsSenderRetries
      - TestSDKCardAPIClosesIdleConnections
  - package: internal/runtime
    tests:
      - TestControllerRetriesProgressAfterShortAttemptTimeout
      - TestControllerQueuesShortStreamDeltaForProgressPatch
  - package: internal/notifier
    tests:
      - TestTaskCardsStreamAndExposeStopOnlyWhileActive
```
