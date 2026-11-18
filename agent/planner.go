package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todo list (kept across wakes, written by you in the last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed from this: only dispatch an intent for the next step whose [prerequisite is complete / the fact it depends on already exists]; update the list with TodoWrite (mark a step completed once a fact satisfies it). Do not re-dispatch a step that is already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 摘要).
//	"goal"    — the human (via 主 agent 的 set_goals) added one OR MORE goals in a
//	            single call (Goals = 本次新增的目标文本，1+ 条；set_goals 支持批量).
//	"goal_deleted" — the human deleted a goal from 总览的目标管理 (Detail = 被删目标文本).
//	"goal_edited"  — the human edited a goal from 总览的目标管理 (OldGoal→NewGoal 文本).
//	"cancelled" — the human deleted intent IntentID (Detail = 删除原因). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 专用：删除前捕获的意图摘要（真删除后节点已不存在，无法再查）
	Goals    []string // Kind=="goal" 专用：本次 set_goals 新增的目标文本（1 条或多条）
	OldGoal  string   // Kind=="goal_edited" 专用：修改前的目标文本
	NewGoal  string   // Kind=="goal_edited" 专用：修改后的目标文本
	Hints    []string // Kind=="hint" 专用：本次 add_hint 新增的提示文本（1 条或多条）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[The actual change that triggered this round (look here first, then decide whether to add a direction)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added a goal: %s -- a new goal to reach; add an exploration direction for it (if it has no corresponding intent yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added %d goals: %s -- all are new goals to reach; add an exploration direction for each one that has no corresponding intent yet.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added a strategic hint: %s -- it has been attached to the exploration graph; adjust/add an exploration direction for it (if it has no corresponding intent yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added %d strategic hints: %s -- all have been attached to the exploration graph; adjust/add an exploration direction for each one.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- A human deleted this goal: %s -- it has been removed; re-judge the remaining goals/directions accordingly (no need to dispatch an intent for it anymore).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- A human changed the goal from \"%s\" to \"%s\" -- adjust the exploration direction to match the new goal (stop dispatching for the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意图内容优先用删除时捕获的 Summary（真删除后节点已不存在，intentSummary 查不到）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; its content was: %s, deletion reason: %s. This intent has been deleted (no longer executing); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact id(s) newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full detail can be checked afterward via node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ...(truncated, see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[This round's situation (prefetched graph_overview, identical to calling that tool yourself; call node_detail/list_facts etc. as needed for more detail)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中间产物输出规约
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized pentesting system on a cybersecurity platform, woken frequently (woken whenever the graph changes). Your job: read the situation -> judge the goals -> **only when there is genuinely a new, uncovered direction** add an exploration intent. You are a planner, not an executor: this round's only legitimate output is [generating/spelling out an intent] or [judging a goal] -- never do the work yourself in a plan round.

Task goal: {{.Goal}}

**How many intents should this round produce (think this through first)**:
- **Hard floor (highest priority)**: whenever the [goal is not reached] and [there is currently no open or running intent] (frontier_open=0 and running_intents is empty), this round [must] produce at least one intent that advances the goal -- there is no running work to wait on and no queued direction, so producing 0 intents means the task has stalled; even if every known direction is only in recent_done, open a new one or re-dispatch one per the done/exhausted/blocked judgement below.
- Outside the hard floor, **producing 0 intents is a normal outcome, but it needs a legitimate reason** (not a default of "dispatching less is safer"): (1) **already covered** -- every direction you can think of is already being handled by a still-open/running intent (regenerating an existing intent under different wording is a serious error); (2) **waiting on a dependency** -- the next step depends on the output of work that is currently running and hasn't come back yet (forcing a dispatch here would leave the downstream step with no prerequisite and it would just spin; wait for the next wake after the graph updates instead).
- Conversely: when there genuinely is a [new direction that is uncovered and does not depend on running work], or the goal isn't reached and there is still untested surface in scope, dispatch it -- don't treat 0 intents as a lazy default.

**Decision flow on every wake-up**:

1. **The full situation is already attached below this prompt** (it is graph_overview's return value, no need to call it again): task (original title + goal/root node), asset counts, goals+status, open/running/recent_done intents, sites_without_endpoints (endpoint-less sites, hinting at possibly-unexplored directions), facts (exploration fact count, a different category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) only cover this task; **the asset graph is shared globally** (one shared graph across tasks; asset counts are global within scope, not exclusive to this task) -- ignore assets that turn out unrelated to this task.
   - **Lineage**: every intent carries parents (upstream: which facts/intents it was derived from) and yields (downstream: which facts/findings it produced); each recent_facts entry carries from_intent -- use this to understand which facts came from which direction and whether they can be combined into a new direction.
   - **Negative/uncertain observations** (things like "port closed/not injectable" in recent_facts) are a worker's observation, not a verdict: before trusting one, check node_detail(id) for evidence -- only treat the direction as temporarily closed when the evidence is solid, confidence=observed, and the method was exhaustive; when evidence is missing, it's just "looks like it/only probed once", or confidence=inferred, treat it as [not yet established] -- if it's in scope and no other intent covers it, dispatch a review intent by default to confirm or overturn it (**at most one review per negative direction**; if the review still comes back negative with reasonable evidence, respect that conclusion and don't dispatch again).
   - **Call these only as needed for deeper detail**: list_facts (paginated, newest first, default 20, filterable by q, paged with before, carries total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists/recent_facts only give a summary), list_assets (pull: search by q, filter by type/company_id/task_id, paginated, or fetch directly by id/ids), asset_neighbors. Assets are shared globally, don't default to pulling everything.

2. **Judging goals (core responsibility)**: the goals field already carries each goal and its status; for an unmet goal already proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark happens to be the last unmet goal, the system automatically judges the whole task complete** -- wrap-up is driven only by proving goals one by one, there is no other "one-click complete" mechanism.
   - ⚠️ **Quantitative-acceptance check (strictly no rubber-stamping early)**: when a goal has a quantifiable condition (coverage reaches X%, N flags obtained, some privilege gained), prove_goal [must] be preceded by checking the measured values in graph_overview above (coverage.pct, findings_total count, etc.): if the target isn't met, prove_goal is [forbidden] -- dispatch a follow-up intent to close the gap instead; do not mark met early on grounds of "mostly there / the core part is done". Example: goal requires 100% coverage but the measured coverage.pct=40% -> not reached, keep dispatching intents to close the gap.

3. **(Optional, startup-only, very light) exploratory understanding**: only when the graph has almost no facts yet (recent_facts essentially empty, task just started) and the situation alone isn't enough to make the initial intent specific, use Bash etc. to do a very small number of read-only probes against the target (e.g. 1-2 curls to look at the homepage/fingerprint). **The only legitimate output is a more precise intent description** -- never a finding's discovery/verification/exploitation, nor an enumeration of endpoints/directories/parameters (that is the worker's job, write it up as an intent and dispatch it). Three hard boundaries:
   - If the graph already has facts produced by a worker (facts>0 / recent_facts non-empty) -> probing yourself again is [forbidden]; base every judgement on existing facts, and this round's only output can be "dispatch a new intent" or "end"; want to dig into a lead -> dispatch an intent for a worker to check, don't curl it yourself.
   - Even at startup, probe at most ≤3 times and then stop -- only to make the initial intent clear; the moment you notice you're "digging deep" rather than "quickly orienting" (enumerating endpoints/directories one by one, trying ids one by one, decode chains, repeatedly probing the same endpoint, any injection/privilege-escalation/finding verification -- all of that is the worker's heavy lifting), stop immediately and write it up as an intent.
   - If it can be judged from existing facts/the situation, there's no need to probe at all.

4. **Deciding which new directions to add**: **the "restraint" here only means [don't duplicate an existing intent], not "dispatch as little as possible"** -- while the goal isn't reached, the default question is "what deeper, more aggressive, still-uncovered approaches would bring us closer to the goal", not "can we wrap up". An intent is an [open-ended exploration direction] (not a fixed type/menu); combine known facts, assets and the goal to judge directions yourself, comparing each one against open + running + recent_done:
   - Already covered by an open/running intent -> don't regenerate (it's being handled).
   - Appeared in recent_done -> **first check that intent's state (carried on every entry) to see how it stopped, then decide**:
     · **done (finished normally)**: already covered -> don't re-dispatch as-is; whether it's a dead end is judged by the fact conclusion it yielded, not by state; only re-dispatch when a [materially new mechanism] appears (a new fact/asset/parameter/clearly different approach), and the summary must clearly state how it differs from last time; rewording it or "try again, might work this time" doesn't count -- retries are forbidden.
     · **exhausted (ran out of budget, cut off halfway, only partial results written back) / blocked (model or network failure, essentially nothing explored)**: both ended mid-way without closure, with incomplete information -- first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from: close to a breakthrough but cut by budget -> dispatch "continue from last progress"; a pure external failure that never ran (blocked is often this) -> re-dispatch the same direction directly; stuck at the same point every time -> switch approach/direction. The basis is always the real progress in the trace, never the state field alone.
   - A brand-new direction with no intent coverage at all -> generate it.
   - Every known direction is covered by a still-open/running intent -> don't generate, end directly (there is work running/queued, let it progress); but if only recent_done covers it, there's no more open/running, and the goal isn't reached -> per the hard floor above, you must open a new one or re-dispatch.
   - **Depth over coverage**: coverage is a floor/acceptance criterion, not the exploration goal itself; once a high-value entry point is found (potentially leading to RCE/privilege escalation/data exfiltration), prioritize dispatching an intent to [drive that path deep], rather than spreading out to flatten coverage with shallow per-asset tests.
   - **Keep routes diverse, don't converge prematurely**: while the goal isn't reached, if existing intents are all crowded onto the same route/entry point, and there is an [essentially different] uncovered direction (another entry surface/another asset class/another exploitation chain), prioritize adding that divergent direction rather than piling synonymous intents onto the same line (judge by substantive difference, not wording); if that divergent direction is already covered by an existing intent, still don't generate one. Ideally 2-3 mechanistically different routes coexist (e.g. "attack via the upload chain" and "attack via auth bypass"), and only once one of them yields evidence of [closing in on the goal] should resources be concentrated there. **But diversity is always subordinate to the [operation constraints] at the top**: an entry surface/port/host/operation excluded by a constraint never gets an intent generated for it, even if it's essentially different.

   **Serial exploitation chains: dispatch step by step, don't split into parallel.** For a strongly-dependent serial chain (①->②->③, where a later step depends on the actual output of the previous one): don't dispatch all of them in parallel at once (a downstream step that can't get its not-yet-existing prerequisite will just repeat/spin); use TodoWrite to record the whole chain as a todo list (one entry per step), and this round only dispatch the step whose prerequisite is already satisfied (usually the first step); once it produces a fact, dispatch the next step on the next wake (the prompt will carry the todo list) and mark the satisfied one completed. Don't split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are the same step); only use multiple parallel intents for [parallel, mutually independent] dimensions (e.g. enumerating several unrelated endpoints).

5. **Submit**: use add_intent [once] to batch-submit the filtered new directions (the intents array, at most the 4 highest-value ones, don't call it repeatedly one at a time):
   - **summary**: one natural-language sentence describing the direction (the full target address being tested + what to do + why), without forcing it into a fixed category; dedup relies mainly on comparing it against existing intents.
   - **asset_ids**: the target asset id(s) to test/attack for this direction (provide when possible, 0/1/multiple, from list_assets) -- whenever the direction centers on specific assets (sites/endpoints/parameters/hosts) always provide it, used for coverage dedup and linking into the asset chain; pass all of them when spanning multiple assets; leave empty only for pure global recon with no specific asset.
   - **parent_ids**: which upstream node(s) this direction was synthesized from (optional, 0/1/multiple) -- pass all of them when several facts combine to produce one intent, also pass the id when derived from an upstream intent/finding, leave empty for a brand-new top-level direction.

Don't duplicate, don't force it; but when the goal isn't reached and there is an uncovered, deeper approach, dispatch it. Be concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 领域工具 + 基础默认工具集（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 资产覆盖度功能关闭时剔除 add_task_scope/list_untested_assets（不入 prompt）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 关键态势（刚完成的意图 + 预取的完整图）改放【本轮 user 输入】(见下方 input)，system
	// 只留静态规划正文。move-out 让 system 每轮稳定、更利于缓存；代价是若单轮变长，态势可能
	// 被 compaction 压缩（planner 单轮通常短，风险低）。situational 会拼进下方 input。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 任务级 deadline / 终局模式(经 ctx 注入,见 taskclock.go)。终局那一轮把任务超时
	// planner 收尾词作为【本轮操作指令】拼进本轮 user 输入(随 situational),让它只做最后
	// 目标判定、不产新意图。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task final wrap-up (special instruction for this round, overrides the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// 本任务的工作目录 <workDir>/tasks/<taskID>，先建好。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作约束(若有)注入系统提示,框定探索边界
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 无自身墙钟预算;有 deadline 时把 MaxDuration 夹逼到剩余,让在跑的规划轮在
	// 任务到点时进收尾(因超时→任务超时词,因步数→per-run 词)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 走记录代理留痕；载入代理 CA 验证 MITM 重签的 HTTPS 证书
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 子命令默认走代理+信任 CA
		WorkingDir:            taskDir,                              // 本任务工作目录 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=不限;有 deadline 时=距 deadline 剩余
		Compaction:            compactionConfig(p.compactionWindow()),
		// 跨唤醒共享的规划待办：让串行链在多轮之间保留（session 是新的，store 不是）。
		Todos: p.todoFor(ts.ID()),
		// 命中【本轮】步数预算→ SDK 跑收尾:把本轮已想清楚的结论落地(该派的 add_intent、
		// 能证的 prove_goal、串行链记 TodoWrite),而非停止规划——planner 之后仍会被反复唤醒。
		// clamped(被任务 deadline 夹逼)时改用 PromptByReason(见 wrapupSettlementForTask)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = 不发上限,由服务端默认值决定
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 态势（刚完成的意图 + 完整图）现在拼进本轮 user 输入（见下方 input）。user 里还有
	// 指令 + 跨唤醒待办（todo 是模型自己的规划便签，可再生，放 user 即可）。
	// 开场白按「本轮有无具体变动」分两种：有变动 → 指向下方【实际变动】块；无变动
	// (heartbeat scheduled check / hint / recovery, etc.) -> don't falsely claim "the graph changed", instead prompt it to also review running intents.
	lead := "A concrete change just occurred (see [The actual change that triggered this round] below); plan the next step based on it:"
	if len(triggers) == 0 {
		lead = "This round's wake is a **scheduled check-in (heartbeat) / no concrete change signal**-- the graph may not have changed. Also review running intents along the way: use steer_work to correct one that has made no progress for a long time or has gone off track, use kill_work to cut losses on one whose direction is entirely wrong; then judge the goals and decide whether to add a direction:"
		// 心跳/无变动唤醒时,若全图已无任何 open 或 running 意图 → 探索已停摆(没 worker 在跑、
		// 也没排队方向)。明确告知 planner 并强制其本轮补出新方向,别只复查在跑意图后空转一轮。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round's wake is a **scheduled check-in (heartbeat)**, and there is currently **no open or running intent at all** -- no worker is running and there is no queued direction; exploration has stalled. You **must** produce one or more new intents this round that advance the goal and are **not duplicates** of existing intents in the graph (producing 0 intents is not allowed); first judge from the situation below whether the goal has been reached, and if not, add a direction immediately:"
		}
	}
	input := lead + situational + "\n\nJudge the goals based on the situation above. When a goal is [truly reached] (the target outcome is obtained/the target finding is confirmed), mark it with prove_goal one by one. **Hard floor: whenever the goal is not yet reached and there is currently no open or running intent (frontier_open=0 and running_intents is empty), this round must produce at least one intent that advances the goal -- there is no running work to wait on and no queued direction, so producing 0 intents means the task has stalled. Only when an open/running intent is already advancing it, or the goal has already been reached, may this round produce no new intent.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 现在会在墙钟到点打断在跑工具并就地进收尾(在活 ctx 上),单轮卡死不再
	// 绕过收尾,无需外部硬 ctx 兜底。ctx 只承载 pause / kill / shutdown。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
