# Feishu Follow-Up Submit Feedback Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Feishu follow-up form submits visibly acknowledged immediately before Codex resume work runs.

**Architecture:** Keep routing decisions in `internal/router`, card wording in `internal/notifier`, and Feishu API details in `internal/transport/feishu`. Task cards become patchable by setting `config.update_multi=true`; resume submits first try to patch the submitted card into a no-form running state, then fall back to sending a new running acknowledgement card if patching fails.

**Tech Stack:** Go 1.26, SQLite store already in place, Feishu `im/v1/messages.patch`, existing fakes and `go test ./...`.

---

## Chunk 1: Patchable Card Transport

### Task 1: Add Update Target to Outbound Messages

**Files:**
- Modify: `internal/contracts/contracts.go`
- Modify: `internal/transport/feishu/sender.go`
- Modify: `internal/transport/feishu/sender_test.go`

- [ ] **Step 1: Write failing sender tests**

Add coverage that task cards include `update_multi: true`, and that an outbound message with an update target calls the fake API's patch path instead of create/reply.

- [ ] **Step 2: Run targeted tests**

Run: `go test ./internal/transport/feishu -run 'TestBuildInteractiveCard|TestSender' -count=1`

Expected: FAIL because update target and patch support do not exist.

- [ ] **Step 3: Implement transport support**

Add `UpdateMessageID string` to `contracts.OutboundMessage`.

Extend `CardAPI` with:

```go
PatchCard(ctx context.Context, messageID string, cardJSON []byte) (retryAfter time.Duration, err error)
```

In `Sender.Send`, if `msg.UpdateMessageID != ""`, call `PatchCard` with the built card. Return `SentMessage{MessageID: msg.UpdateMessageID}` on success so callers can continue to route the same card.

In `SDKCardAPI.PatchCard`, use `larkim.NewPatchMessageReqBodyBuilder().Content(string(cardJSON)).Build()` and `client.Im.Message.Patch`.

Set card config to:

```json
{"wide_screen_mode": true, "update_multi": true}
```

- [ ] **Step 4: Re-run targeted tests**

Run: `go test ./internal/transport/feishu -count=1`

Expected: PASS.

## Chunk 2: Follow-Up Feedback Notifier

### Task 2: Add No-Form Running Feedback Card

**Files:**
- Modify: `internal/notifier/notifier.go`
- Modify: `internal/notifier/notifier_test.go`

- [ ] **Step 1: Write failing notifier tests**

Add tests for a `FollowUpAccepted` notifier method:

- with `UpdateMessageID`, it sends a running task card update.
- with no update target, it sends a running acknowledgement card.
- neither rendering includes the follow-up form action.
- the body includes `已收到继续跟进`, submission time, and redacted prompt summary.

- [ ] **Step 2: Run notifier tests**

Run: `go test ./internal/notifier -count=1`

Expected: FAIL because the method and no-form card rendering do not exist.

- [ ] **Step 3: Implement notifier support**

Add a `FollowUpAcceptedInput` with chat id, update message id, task metadata, prompt, and timestamp.

Add `FollowUpAccepted(ctx, in)` to build a `CardStart` style outbound message with:

- title `继续处理中 · <task_id>`
- status `running`
- no actions
- fields matching existing task cards
- body confirming receipt and showing a short redacted prompt summary.

- [ ] **Step 4: Re-run notifier tests**

Run: `go test ./internal/notifier -count=1`

Expected: PASS.

## Chunk 3: Router Resume Feedback

### Task 3: Send Feedback Before Running Resume

**Files:**
- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`
- Modify: `internal/app/integration_test.go`

- [ ] **Step 1: Write failing router tests**

Add tests that:

- card follow-up submit calls `FollowUpAccepted` before `Runner.Resume`;
- if the update feedback errors, router sends fallback acknowledgement before resume;
- if both feedback attempts fail, resume still runs and result send behavior is unchanged.

- [ ] **Step 2: Run router tests**

Run: `go test ./internal/router -count=1`

Expected: FAIL because router does not send feedback before resume.

- [ ] **Step 3: Implement router behavior**

After `AdmitResumeRun` succeeds and before `runner.Resume`, call:

```go
r.sendFollowUpAccepted(ctx, ev, admit.Task, text)
```

For card actions, first attempt to update `ev.RootMessageID`. If that fails or `RootMessageID` is empty, send a new acknowledgement card in the chat. Do not block the resume on feedback failure; preserve the error and return it only if the final result send succeeds and there is no more important error.

- [ ] **Step 4: Re-run router and integration tests**

Run: `go test ./internal/router ./internal/app -count=1`

Expected: PASS.

## Chunk 4: Full Verification and Service Restart

### Task 4: Verify, Build, Restart

**Files:**
- Modify as needed from earlier tasks only.

- [ ] **Step 1: Run full test suite**

Run: `go test ./... -count=1`

Expected: PASS.

- [ ] **Step 2: Build binary**

Run: `go build -o bin/codex-feishu-bridge ./cmd/codex-feishu-bridge`

Expected: PASS.

- [ ] **Step 3: Restart launch agent**

Run: `launchctl kickstart -k gui/501/info.fofa.codex-feishu-bridge`

Expected: launch agent is running with the rebuilt binary.

- [ ] **Step 4: Confirm service state**

Run: `launchctl print gui/501/info.fofa.codex-feishu-bridge`

Expected: `state = running` and a current PID.

- [ ] **Step 5: Send required Feishu task notification**

Run `~/.codex/bin/feishu_notify` with topic, progress, project, and summary.
