package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Finding ID convention**: finding_id is the standalone finding record ID; finding_node_id is the exploration node ID. The id field in list_findings / list_task_findings / node_detail / get_task_node_detail stays the exploration node ID; read the standalone ID from the finding_id in that same response. get_finding_traffic / bind_finding_traffic use the standalone finding_id. The legacy update_finding_report's finding_id parameter still takes finding_node_id. Don't use the number from report_finding's first line for evidence tools, and don't guess another number after hitting an ID error."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default, traffic is verified and bound by the report-writing agent before it writes the report. The reporter should keep the verification command, key output, and any existing real traffic ID(s) with their roles in evidence, for the report agent to cross-check against the execution record; no need to search for packets just to bind. Explicit immediate binding is still supported: traffic_refs or evidence_hint_id can submit already-verified references, with the latter reading the structured references of a specified hint in this task; if either is invalid, this report fails entirely. TCP/no-capture cases don't need these optional parameters. The returned finding_id and finding_node_id represent the standalone record and the exploration node, respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing off a confirmed finding, keep the verified traffic's ID, role, note, and order in the corresponding hint's traffic_refs (at the top level for a single one, in the matching hints element for a batch), and state in text which specific finding it proves. The caller must not hand off only text and discard existing traffic references. An unverified candidate must not be passed as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence handoff (optional)**: auto-binding is, by default, completed by the report-writing agent after the finding is saved and before the report is written. The reporter should keep the verification command, key output, and existing real traffic ID(s) with their roles in evidence, with intent_id in the task, to make it easy for the report agent to trace back; no need to search for packets extra just to bind. When Auto / Planner reports on behalf of an executor, don't discard the executor's existing references. add_hint / add_task_hint can hand off references via traffic_refs; explicit immediate binding via report_finding's traffic_refs / evidence_hint_id is still supported. For TCP or no-capture cases, register normally -- don't guess IDs, and don't re-probe just to fill this in."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen a platform conversation has no task context, don't call report_finding directly; hand off via add_task_hint to the relevant existing task, let the task agent register it, and verify the result with list_task_findings."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore judging a goal complete, first finish reporting/handing off this run's existing evidence. Don't end the task or cancel the worker just because a finding has been textually registered while evidence handoff is still incomplete; a no-capture case does not require waiting or forcibly capturing packets."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Auto-link traffic before the report (enabled)**: you are responsible for verifying and binding traffic for the finding that triggered this run, then writing the report. First get a definite finding_id and finding_node_id from report_finding's returned JSON, or from get_task_node_detail / list_task_findings. Read the finding's detail, the corresponding intent's execution record, and the existing evidence list, preferring any real ID handed off by the reporter. If this verification was HTTP and traffic tools are available, filter candidates with traffic_search, then individually verify with traffic_get that the request/response genuinely supports the finding; domain and time are for filtering only, they don't prove ownership. Link the confirmed evidence in reproduction order with bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and stating its role. Only operate on this finding -- don't create a duplicate finding or re-probe the target. After a successful bind, call get_finding_traffic again to get the latest version, read the needed body, then pass the version actually read as evidence_version to update_finding_report (whose finding_id parameter still takes finding_node_id). An existing binding need not be re-added. For TCP, uncaptured cases, unavailable tools, or no exact match, skip auto-binding, write the report normally from the text/command evidence and state why, and never guess just to fill out the traffic. Don't claim success on a failed bind; keep the existing evidence and explain in the report why nothing was bound."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references corresponding to the specific finding in this hint, order preserved; after handoff, report_finding can pass evidence_hint_id to carry these references.", "items": obj(map[string]any{"traffic_id": str("real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("what conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node of this task (an inherited hint cannot be used directly for binding)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
