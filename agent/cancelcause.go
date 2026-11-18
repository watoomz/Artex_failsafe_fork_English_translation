package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "the user paused the task",
		"the user paused the task via the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; running intents are returned to the frontier (open) and re-claimed and executed from scratch once the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "the orchestration agent paused the task",
		"the orchestration agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; running intents are returned to the frontier (open) and re-executed once resumed")
	AbortTaskDeleted = cause("task_deleted", "the task was deleted",
		"the task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled this task's running Planner, Worker and main agent; this run's result will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "the backend restored the task's paused state",
		"on startup, the backend restored the task's paused state from what was persisted in the database. This run was cancelled; normally there should be no agent running during the restore phase")
	AbortGoalMet = cause("goal_met", "the planner judged the task goal reached",
		"the planner judged the task goal reached and set the task to done, then cancelled any still-running Worker; these intents are marked stopped, not failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "the task's timeout wrap-up grace period ran out",
		"after the task hit timeout, it waited for the running Worker to wrap up gracefully, but even the 90-second drain grace period wasn't enough, so a hard cancel was performed; the intent is marked exhausted, and any facts/assets already written during wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "the planner terminated this intent",
		"the planner called kill_work to actively terminate this intent, usually meaning the direction went off track or is no longer worth pursuing; the intent is marked stopped and will not be auto-reclaimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "the user paused this Worker intent",
		"the user paused the running Worker. This call was cancelled and the intent moved to paused; all already-registered intents, facts, findings and activity records are kept, and it executes from scratch again once resumed")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "the user deleted this Worker intent",
		"the user deleted the running Worker. This call was cancelled; once the Worker exits the write zone, the server handles the intent per the user's chosen delete mode -- a soft delete only marks it deleted and keeps all output, a hard delete cascades to remove the intent and any downstream nodes supported only by it")
	AbortWorkFinished = cause("work_finished", "the Worker finished normally and released its context",
		"the Worker finished normally, and the engine released its context resources in detachWork. This is not an interruption; if it shows up in an interrupt message, the cancellation and the finishing event raced")
	AbortPausedRaceGuard = cause("paused_race_guard", "refused to start a new run while the task is paused",
		"while the task is paused, the engine refuses to issue a new execution context, to prevent a race between claim and pause from letting a Worker start anyway; a claimed intent is returned to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "the user stopped this conversation turn",
		"the user clicked stop, actively aborting this run of the main agent or session agent. Already-produced activity records are kept, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "the task was paused, aborting the main-agent conversation",
		"when the user paused the task, the running main-agent conversation was cancelled along with it. Already-produced activity records are kept; this turn's message is not auto-replayed once the task resumes")
	AbortChatTurnFinished = cause("chat_turn_finished", "this conversation turn finished normally and released its context",
		"this conversation turn finished normally, and the server is releasing that turn's context resources. This is not an interruption; if it shows up in an interrupt message, the cancellation and the finishing event raced")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "the backend process is shutting down",
		"the backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents are cancelled; after restart, any leftover running intents are reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "the hard-timeout fallback for a single run triggered",
		"a single run exceeded its soft wall-clock budget plus extra grace, meaning a model request or some tool didn't return for a long time, preventing normal turn-boundary wrap-up from running. Check especially the last unreturned tool call before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "an upstream context hit its deadline",
			"an upstream context hit its deadline, but whoever set it did not attach a named reason via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "the canceller did not attach a named reason",
			"an upstream context was cancelled, but the canceller did not attach a named reason via context.WithCancelCause; register a reason in agent/cancelcause.go and wire it into that cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
