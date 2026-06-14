# Feishu Follow-Up Submit Feedback Design

## Goal

When a user submits the task card follow-up form in Feishu, the product must make the submission visible immediately. The user should not need to infer success from a new message appearing later, and repeated clicks should not feel necessary.

## Recommended Product Behavior

1. User enters text in the task card form and clicks `继续跟进`.
2. The bridge immediately attempts to update the submitted card to a running state:
   - title: `继续处理中 · <task_id>`
   - body: confirms the follow-up was received, shows submission time, and includes a short redacted summary of the submitted text.
   - actions: no follow-up form while that submission is running.
3. If updating the original card fails, the bridge sends a new running acknowledgement card in the same chat as a fallback.
4. The bridge then runs `codex exec resume`.
5. On completion, the bridge sends a new result card with the normal follow-up input form, preserving the conversation history and enabling the next turn.

## Rationale

The user's attention remains on the card they just clicked. Updating that card is the clearest confirmation that the click was accepted. A new result card is still useful for task history, but it should not be the first or only feedback because it can be delayed, folded into the chat stream, or missed visually.

## Scope

In scope:

- Card action follow-up submits from task cards.
- Immediate running-state feedback before Codex resume starts.
- Best-effort original-card update, with fallback running acknowledgement card.
- Result cards continue to include the follow-up form.
- Tests covering successful update, fallback acknowledgement, and resume continuation.

Out of scope:

- Changing Feishu client behavior such as clearing the input field locally.
- Persisting detailed per-submit UI state beyond existing task/run records.
- Changing shortcut or project selection card flows unless they reuse task-card resume.

## Components

- `transport/feishu`: expose card update support through the card API and sender.
- `notifier`: add a running follow-up feedback card shape that can be rendered without a follow-up form.
- `router`: after admitting a resume run, send the immediate feedback before invoking the runner.
- `store`: keep existing task, run, route, and dedup behavior. No schema change is required.

## Error Handling

- If the resume cannot be admitted, the existing rejection cards remain the only response.
- If the original-card update fails, send a new running acknowledgement card.
- If both feedback attempts fail, continue the resume and return the feedback error only after the run result has been recorded, so Codex work is not lost.
- Result card send errors must still propagate so service logs and tests can expose missed final replies.

## Tests

- Router test: card follow-up sends immediate running feedback before `Runner.Resume`.
- Router test: feedback update failure falls back to a new running acknowledgement card.
- Sender test: update requests call Feishu's update API when a target message id is supplied.
- Existing `go test ./...` remains the acceptance gate.
