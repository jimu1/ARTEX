package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// taskSeqN returns a unique suffix for task names, mirroring the pattern in
// blackboard_inheritance_test.go.
func taskSeqN() int64 { return time.Now().UnixNano() }

// TestGraphOverviewPendingFacts 验证态势块新增的 pending_facts/pending_total：
// 未消费线索最老在前、与 recent_facts 窗口去重、origin 不入、被引用后离队。
func TestGraphOverviewPendingFacts(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions(
		fmt.Sprintf("pending-overview-%d", taskSeqN()), "test pending facts window", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	es := d.Exploration(task.ExplorationID)
	tools := NewToolSet(es, "planner")
	tools.SetTaskID(task.ID)

	// 造 24 条事实：idx0、idx1 两条最老的故意不引用（饿死线索），
	// idx2..23 逐条被意图引用（已消费）。recent_facts 窗口（最新 20）只装得下
	// idx4..23——idx0/idx1 沉在窗口外，正是 pending_facts 要捞的。
	var starved0, starved1 int64
	consumer, err := es.AddIntent(map[string]any{"summary": "consumer"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		id, err := es.AddNode(db.KindFact, map[string]any{
			"summary":    fmt.Sprintf("事实#%d", i),
			"confidence": "observed",
		}, 5, "confirmed", "worker-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			starved0 = id
		case 1:
			starved1 = id
		default:
			if err := es.Link(id, db.RelDerivedFrom, consumer); err != nil {
				t.Fatal(err)
			}
		}
	}

	overview := tools.graphOverviewData()

	pending, ok := overview["pending_facts"].([]map[string]any)
	if !ok {
		t.Fatalf("pending_facts missing or wrong type: %T", overview["pending_facts"])
	}
	if len(pending) != 2 {
		t.Fatalf("pending_facts want 2 entries, got %d: %+v", len(pending), pending)
	}
	if pending[0]["id"].(int64) != starved0 || pending[1]["id"].(int64) != starved1 {
		t.Fatalf("pending_facts want oldest-first [%d,%d], got %+v", starved0, starved1, pending)
	}
	// 条目形状与 recent_facts 一致：confidence 在、summary 在。
	if pending[0]["confidence"] != "observed" || pending[0]["summary"] == "" {
		t.Fatalf("pending entry shape wrong: %+v", pending[0])
	}
	// 配对计数：全量未消费总数（=2），与窗口条数解耦。
	if overview["pending_total"] != 2 {
		t.Fatalf("pending_total want 2, got %v", overview["pending_total"])
	}
	// 与 recent_facts 去重：饿死线索已沉出窗口，窗口内不得重复出现 pending 条目。
	recent, _ := overview["recent_facts"].([]map[string]any)
	for _, r := range recent {
		if r["id"].(int64) == starved0 || r["id"].(int64) == starved1 {
			t.Fatalf("starved fact leaked into recent_facts window: %+v", r)
		}
	}

	// 引用 starved0（新意图 parent）后离队：pending 只剩 starved1。
	if err := es.Link(starved0, db.RelDerivedFrom, consumer); err != nil {
		t.Fatal(err)
	}
	overview = tools.graphOverviewData()
	pending, _ = overview["pending_facts"].([]map[string]any)
	if len(pending) != 1 || pending[0]["id"].(int64) != starved1 {
		t.Fatalf("after citing starved0, pending want [%d], got %+v", starved1, pending)
	}
	if overview["pending_total"] != 1 {
		t.Fatalf("pending_total after cite want 1, got %v", overview["pending_total"])
	}
}

// TestGraphOverviewPendingFactsYoungGraphDedup 任务早期（事实少）全部事实都在
// recent_facts 窗口里：pending_facts 与之去重后不出现，避免双列同一批。
func TestGraphOverviewPendingFactsYoungGraphDedup(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions(
		fmt.Sprintf("pending-young-%d", taskSeqN()), "young graph dedup", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	es := d.Exploration(task.ExplorationID)
	tools := NewToolSet(es, "planner")
	tools.SetTaskID(task.ID)

	for i := 0; i < 3; i++ {
		if _, err := es.AddNode(db.KindFact, map[string]any{"summary": fmt.Sprintf("未引用#%d", i)}, 5, "confirmed", "worker-1", nil); err != nil {
			t.Fatal(err)
		}
	}

	overview := tools.graphOverviewData()
	if _, has := overview["pending_facts"]; has {
		t.Fatalf("young graph: pending_facts should be deduped away (all facts already in recent_facts), got %+v", overview["pending_facts"])
	}
	if _, has := overview["pending_total"]; has {
		t.Fatalf("young graph: pending_total should be absent alongside pending_facts")
	}
	// recent_facts 照常显示全部 4 条（任务根 origin + 3 条未引用），可见性不受影响。
	if recent, _ := overview["recent_facts"].([]map[string]any); len(recent) != 4 {
		t.Fatalf("recent_facts want 4 (origin + 3), got %d", len(recent))
	}
}
