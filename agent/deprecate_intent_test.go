package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func callToolText(t *testing.T, tool actool.CoreTool, input string) string {
	t.Helper()
	result, err := tool.Call(context.Background(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	return result.Flatten()
}

// TestDeprecateIntentTool 覆盖四态：open 成功落墓碑 / running 走引擎停工通道且原因先落 /
// 终态与 paused 报错 / 空 reason 拒。
func TestDeprecateIntentTool(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions(
		fmt.Sprintf("deprecate-tool-%d", time.Now().UnixNano()), "test deprecate_intent tool", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	es := d.Exploration(task.ExplorationID)
	tools := NewToolSet(es, "planner")
	tools.SetTaskID(task.ID)

	var killed []int64
	tools.killWork = func(intentID int64) error { // 引擎停工通道桩
		killed = append(killed, intentID)
		return nil
	}

	tool := tools.deprecateIntent()

	// ① open：写原因 + CAS 落 stopped。
	openID, err := es.AddIntent(map[string]any{"summary": "排队中的死方向"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	out := callToolText(t, tool, fmt.Sprintf(`{"intent_id":%d,"reason":"前提已被事实#7推翻"}`, openID))
	if !strings.Contains(out, "已废弃") {
		t.Fatalf("open deprecate output: %s", out)
	}
	n, _ := es.GetNode(openID)
	if n == nil || n.State != "stopped" || n.StopReason != "前提已被事实#7推翻" {
		t.Fatalf("open deprecate state/reason wrong: %+v", n)
	}

	// ② running：先落原因，再经引擎通道（桩记录被调）。
	runID, err := es.AddIntent(map[string]any{"summary": "跑偏的活方向"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(runID, "open", "running"); err != nil || !ok {
		t.Fatal(err)
	}
	out = callToolText(t, tool, fmt.Sprintf(`{"intent_id":%d,"reason":"跑偏死循环，止损；方向待重估"}`, runID))
	if !strings.Contains(out, "终止信号") {
		t.Fatalf("running deprecate output: %s", out)
	}
	if len(killed) != 1 || killed[0] != runID {
		t.Fatalf("engine kill not routed: %+v", killed)
	}
	n, _ = es.GetNode(runID)
	if n.StopReason != "跑偏死循环，止损；方向待重估" {
		t.Fatalf("running stop_reason missing: %+v", n)
	}

	// ③ 终态（done）拒绝。
	doneID, err := es.AddIntent(map[string]any{"summary": "已完成的意图"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(doneID, "open", "running"); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(doneID, "running", "done"); err != nil || !ok {
		t.Fatal(err)
	}
	out = callToolText(t, tool, fmt.Sprintf(`{"intent_id":%d,"reason":"x"}`, doneID))
	if !strings.Contains(out, "终态") {
		t.Fatalf("terminal deprecate should error: %s", out)
	}

	// ④ 空 reason 拒。
	out = callToolText(t, tool, fmt.Sprintf(`{"intent_id":%d,"reason":"  "}`, openID))
	if !strings.Contains(out, "reason 必填") {
		t.Fatalf("empty reason should error: %s", out)
	}
}

// TestGraphOverviewDeprecatedIntents 断言态势块：墓碑清单形状（含 stop_reason）与计数、
// stopped 不进 recent_done、人类软删（deleted）两边都不进。
func TestGraphOverviewDeprecatedIntents(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions(
		fmt.Sprintf("deprecate-overview-%d", time.Now().UnixNano()), "test deprecated tombstones", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	es := d.Exploration(task.ExplorationID)
	tools := NewToolSet(es, "planner")
	tools.SetTaskID(task.ID)

	// 两条墓碑（一有一无原因）+ 一条 done + 一条人类软删。
	mk := func(summary string) int64 {
		id, err := es.AddIntent(map[string]any{"summary": summary}, 5, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	t1 := mk("否决墓碑")
	if err := es.SetIntentStopReason(t1, "前提已被事实#9推翻"); err != nil {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(t1, "open", "stopped"); err != nil || !ok {
		t.Fatal(err)
	}
	t2 := mk("旧版急停（无原因）")
	if ok, err := es.CompareAndSetIntentState(t2, "open", "stopped"); err != nil || !ok {
		t.Fatal(err)
	}
	doneID := mk("正常完成")
	if ok, err := es.CompareAndSetIntentState(doneID, "open", "running"); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(doneID, "running", "done"); err != nil || !ok {
		t.Fatal(err)
	}
	delID := mk("人类删除")
	if _, err := es.SoftDeleteIntent(delID, "用户手动删除"); err != nil {
		t.Fatal(err)
	}

	overview := tools.graphOverviewData()

	deps, ok := overview["deprecated_intents"].([]map[string]any)
	if !ok {
		t.Fatalf("deprecated_intents missing: %T", overview["deprecated_intents"])
	}
	if len(deps) != 2 {
		t.Fatalf("deprecated_intents want 2, got %d: %+v", len(deps), deps)
	}
	// 最新在前：t2 先、t1 后；t1 带 stop_reason，t2 无该字段（NULL 兼容）。
	if deps[0]["id"].(int64) != t2 || deps[1]["id"].(int64) != t1 {
		t.Fatalf("deprecated order want newest-first [t2,t1]: %+v", deps)
	}
	if deps[1]["stop_reason"] != "前提已被事实#9推翻" {
		t.Fatalf("t1 stop_reason missing: %+v", deps[1])
	}
	if _, has := deps[0]["stop_reason"]; has {
		t.Fatalf("legacy stopped must omit stop_reason field: %+v", deps[0])
	}
	if overview["deprecated_total"] != 2 {
		t.Fatalf("deprecated_total want 2, got %v", overview["deprecated_total"])
	}

	// stopped 不进 recent_done（执行结果清单保持 done/blocked/exhausted）；deleted 两边都不进。
	recent, _ := overview["recent_done_intents"].([]map[string]any)
	for _, r := range recent {
		rid := r["id"].(int64)
		if rid == t1 || rid == t2 {
			t.Fatalf("stopped leaked into recent_done: %+v", r)
		}
		if rid == delID {
			t.Fatalf("deleted leaked into recent_done: %+v", r)
		}
	}
	for _, dp := range deps {
		if dp["id"].(int64) == delID || dp["id"].(int64) == doneID {
			t.Fatalf("non-stopped leaked into deprecated_intents: %+v", dp)
		}
	}
}

// TestPlannerToolsHaveDeprecate 断言动词替换：含 deprecate_intent、不含 kill_work。
func TestPlannerToolsHaveDeprecate(t *testing.T) {
	ts := NewToolSet(nil, "planner")
	names := map[string]bool{}
	for _, tool := range ts.PlannerTools() {
		names[tool.Name()] = true
	}
	if !names["deprecate_intent"] {
		t.Fatalf("planner tools missing deprecate_intent: %v", names)
	}
	if names["kill_work"] {
		t.Fatalf("kill_work should be removed from planner tools: %v", names)
	}
}
