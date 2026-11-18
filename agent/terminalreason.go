package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "the model finished this turn normally but left no text summary; facts and assets go by this turn's tool-call records",
	harness.ReasonMaxTurns:          "hit the turn-count limit (MaxTurns): the SDK has wrapped up and written back facts and assets; the intent is marked exhausted for the planner to continue with a different direction, not treated as a failure",
	harness.ReasonTimeout:           "hit this run's wall-clock budget (MaxDuration): once it's up, the running tool is interrupted and wrap-up happens on the spot, writing back identified facts and assets; the intent is marked exhausted",
	harness.ReasonModelError:        "the model or API call failed (network, auth, rate limit, provider 5xx, etc.); once retries are exhausted the intent is marked blocked -- a transport-layer failure meant this intent essentially never got explored; check its execution trace (get_worker_trace) before deciding to re-dispatch or change approach",
	harness.ReasonBlockingLimit:     "context length hit the hard limit; the request was blocked before being sent; narrow the intent's granularity or compress tool output",
	harness.ReasonPromptTooLong:     "the prompt is too long and context-compression retries have been exhausted; cannot continue executing",
	harness.ReasonImageError:        "the current model does not support this turn's multimodal content; switch to a vision-capable model or avoid tools returning images",
	harness.ReasonStopHookPrevented: "a Stop hook prevented this turn from ending and it could not continue afterward; check whether the task's Guard rules are too strict",
	harness.ReasonHookStopped:       "a tool or hook actively stopped execution, e.g. an out-of-scope target or a forbidden command; check the last tool_result's block explanation",
	harness.ReasonAbortedStreaming:  "the run was cancelled during the model's streaming-output phase",
	harness.ReasonAbortedTools:      "the run was cancelled during the tool-execution phase",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "could not obtain the cancellation reason"
		}
		stage := "during execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "the model-output phase"
		case harness.ReasonAbortedTools:
			stage = "the tool-execution phase"
		}
		sum = "(run interrupted: " + short + "; stopped during " + stage + progressSuffix(term, tr) + ", incomplete)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(hit the run budget limit (" + string(reason) + "), wrapped up and wrote back facts" + progressSuffix(term, tr) + "; no text summary this time)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(no text summary, terminal state " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Interrupt reason** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Interrupt reason**: unavailable; the canceller may not have attached a named reason via context.WithCancelCause\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **Partial output generated before cancellation**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Executed**: %d model turn(s)\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **This run's duration**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Cumulative tokens**: input %d / output %d / cache read %d / cache write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: this run ended before issuing any tool call\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool executing at the time of interruption**: `%s` (running for %s, **no result returned**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before interruption**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "the run's context was cancelled, but the underlying layer produced no Terminal event"
	}
	return "unknown terminal state; the harness may have added a new TerminalReason, please add it to reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turn(s)", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", ran for " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
