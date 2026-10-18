package db

import (
	"fmt"
	"testing"
)

func TestExplorationFlow(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "拿下测试目标")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) // cascades nodes/edges/activity
	es := d.Exploration(expID)

	// goal node + two intents
	goal, err := es.AddGoal(map[string]any{"text": "getadmin", "vulnclass": "authz"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AddIntent(map[string]any{"summary": "enumerate endpoints"}, 5, nil, "planner"); err != nil {
		t.Fatal(err)
	}
	i2, err := es.AddIntent(map[string]any{"summary": "test idor"}, 8, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}

	// frontier ordered by priority desc → i2(8) before i1(5)
	fr, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr) != 2 || fr[0].ID != i2 {
		t.Fatalf("frontier order wrong: %+v", fr)
	}

	// atomic claim: first wins, second on same id fails
	ok, err := es.ClaimIntent(i2, "worker-1")
	if err != nil || !ok {
		t.Fatalf("claim i2: ok=%v err=%v", ok, err)
	}
	ok2, _ := es.ClaimIntent(i2, "worker-2")
	if ok2 {
		t.Fatalf("double-claim should fail")
	}

	// finding yields from intent, proves goal
	find, err := es.AddNode("finding", map[string]any{"vulnclass": "idor", "severity": "high"}, 9, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(i2, "yields", find); err != nil {
		t.Fatal(err)
	}
	if err := es.Link(find, "proves", goal); err != nil {
		t.Fatal(err)
	}
	if err := es.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}

	// lineage: ancestors of the finding traced backward — here {i2, find} joined by
	// the yields edge. The proves→goal edge is DOWNSTREAM (goal must be excluded),
	// and the unrelated intent i1 is not on any path to the finding (excluded too).
	lnNodes, lnEdges, err := es.FindingLineage(find)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, n := range lnNodes {
		got[n.ID] = true
	}
	if len(lnNodes) != 2 || !got[find] || !got[i2] {
		t.Fatalf("lineage nodes: want {i2,find}, got %+v", lnNodes)
	}
	if got[goal] {
		t.Fatalf("lineage must exclude the proved goal (it is downstream of the finding)")
	}
	if len(lnEdges) != 1 || lnEdges[0].From != i2 || lnEdges[0].To != find || lnEdges[0].Rel != "yields" {
		t.Fatalf("lineage edges: want i2-yields->find, got %+v", lnEdges)
	}

	// activity poll by id cursor
	id1, err := es.AppendActivity(Activity{Worker: "worker-1", Kind: "tool_use", Tool: "Bash", Summary: "ran curl", Detail: "full output"})
	if err != nil {
		t.Fatal(err)
	}
	items, cursor, err := es.ActivityList(nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || cursor != id1 {
		t.Fatalf("activity list: items=%d cursor=%d", len(items), cursor)
	}
	det, _ := es.ActivityDetail(id1)
	if det != "full output" {
		t.Fatalf("detail want 'full output', got %q", det)
	}
	// incremental: nothing new after cursor
	items2, _, _ := es.ActivityList(nil, cursor, 100)
	if len(items2) != 0 {
		t.Fatalf("incremental poll should be empty, got %d", len(items2))
	}

	// stats
	st, _ := es.Stats()
	if st["intent"] != 2 || st["goal"] != 1 || st["finding"] != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestIntentPauseResumeAndCancelCleanup(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "worker control cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{
		Domain: fmt.Sprintf("cancel-intent-%d.invalid", expID),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, assetID)

	otherIntent, err := es.AddIntent(map[string]any{"summary": "keep intent"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := es.AddIntent(map[string]any{"summary": "cancel intent"}, 10, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-control-test"); err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "paused"); err != nil {
		t.Fatal(err)
	}

	frontier, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range frontier {
		if node.ID == intentID {
			t.Fatalf("paused intent %d must not enter frontier", intentID)
		}
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-while-paused"); err != nil || claimed {
		t.Fatalf("paused intent claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "open"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(intentID, "worker-after-resume"); err != nil || !claimed {
		t.Fatalf("resumed intent claim: claimed=%v err=%v", claimed, err)
	}
	if err := es.SetIntentState(intentID, "paused"); err != nil {
		t.Fatal(err)
	}

	directFact, err := es.AddNode("fact", map[string]any{"summary": "remove fact"}, 0, "confirmed", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	directFinding, err := es.AddNode("finding", map[string]any{"summary": "remove finding"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	keptFact, err := es.AddNode("fact", map[string]any{"summary": "keep fact"}, 0, "confirmed", "other-worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(intentID, RelYields, directFact); err != nil {
		t.Fatal(err)
	}
	if err := es.Link(intentID, RelYields, directFinding); err != nil {
		t.Fatal(err)
	}
	findingRowID, err := es.AddStandaloneFinding(0, directFinding, "test", "cancelled finding", SeverityHigh, "summary", "evidence", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	activityID, err := es.AppendActivity(Activity{NodeID: &intentID, Worker: "worker", Kind: "result", Summary: "remove activity"})
	if err != nil {
		t.Fatal(err)
	}
	keptActivityID, err := es.AppendActivity(Activity{NodeID: &otherIntent, Worker: "other-worker", Kind: "result", Summary: "keep activity"})
	if err != nil {
		t.Fatal(err)
	}

	cleanup, err := es.CancelIntent(intentID)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 1 || cleanup.Facts != 1 || cleanup.Findings != 1 || cleanup.Activities != 1 {
		t.Fatalf("unexpected cleanup counts: %+v", cleanup)
	}
	for _, nodeID := range []int64{intentID, directFact, directFinding} {
		node, err := es.GetNode(nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if node != nil {
			t.Fatalf("node %d survived intent cancellation", nodeID)
		}
	}
	for _, nodeID := range []int64{otherIntent, keptFact} {
		node, err := es.GetNode(nodeID)
		if err != nil || node == nil {
			t.Fatalf("unrelated node %d removed: node=%v err=%v", nodeID, node, err)
		}
	}

	assertCount := func(query string, want int, args ...any) {
		t.Helper()
		var got int
		if err := d.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("query count=%d, want %d: %s", got, want, query)
		}
	}
	assertCount(`SELECT COUNT(*) FROM findings WHERE id=$1`, 0, findingRowID)
	assertCount(`SELECT COUNT(*) FROM activity WHERE id=$1`, 0, activityID)
	assertCount(`SELECT COUNT(*) FROM activity WHERE id=$1`, 1, keptActivityID)
	assertCount(`SELECT COUNT(*) FROM exploration_edges WHERE exploration_id=$1 AND (src_id=$2 OR dst_id=$2)`, 0, expID, intentID)
	assertCount(`SELECT COUNT(*) FROM exploration_anchors WHERE node_id=$1`, 0, directFact)
	assertCount(`SELECT COUNT(*) FROM assets WHERE id=$1`, 1, assetID)
}

// TestNodesPageQueryMatchesID verifies the 播报板 search filters on node id (both
// the bare number and the「#id」form the UI shows) in addition to payload/origin.
func TestNodesPageQueryMatchesID(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "id 搜索")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	target, err := es.AddNode("fact", map[string]any{"summary": "needle-alpha"}, 0, "confirmed", "worker-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := es.AddNode("fact", map[string]any{"summary": "unrelated-beta"}, 0, "confirmed", "worker-b", nil)
	if err != nil {
		t.Fatal(err)
	}

	onlyTarget := func(label, q string) {
		t.Helper()
		nodes, total, err := es.NodesPage(NodeFilter{Query: q}, 1, 50)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if total != 1 || len(nodes) != 1 || nodes[0].ID != target {
			t.Fatalf("%s: q=%q total=%d nodes=%+v, want single node %d", label, q, total, nodes, target)
		}
	}

	onlyTarget("bare id", fmt.Sprint(target))
	onlyTarget("hash id", "#"+fmt.Sprint(target))
	onlyTarget("payload still works", "needle-alpha")

	// A non-matching numeric id returns nothing (and does not accidentally match other).
	if nodes, total, err := es.NodesPage(NodeFilter{Query: fmt.Sprint(target + other + 1000)}, 1, 50); err != nil {
		t.Fatal(err)
	} else if total != 0 || len(nodes) != 0 {
		t.Fatalf("non-existent id: total=%d nodes=%+v, want empty", total, nodes)
	}
}

func TestPendingFacts(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "pending facts")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) // cascades nodes/edges
	es := d.Exploration(expID)

	// origin fact（任务根，应被排除）
	if _, err := es.AddNode("fact", map[string]any{"summary": "任务起点"}, 0, "origin", "system", nil); err != nil {
		t.Fatal(err)
	}
	// 两条未引用事实（老→新）
	f1, err := es.AddNode("fact", map[string]any{"summary": "线索A"}, 5, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := es.AddNode("fact", map[string]any{"summary": "线索B"}, 5, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 一条将被意图引用的事实 + 引用它的意图（addOneIntent 的 Link 路径）
	f3, err := es.AddNode("fact", map[string]any{"summary": "已消费"}, 5, "confirmed", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	it, err := es.AddIntent(map[string]any{"summary": "consume f3"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(f3, RelDerivedFrom, it); err != nil {
		t.Fatal(err)
	}

	// 只含未引用事实、最老在前、不含 origin。
	pfs, err := es.PendingFacts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pfs) != 2 || pfs[0].ID != f1 || pfs[1].ID != f2 {
		t.Fatalf("pending facts want [f1,f2] oldest-first, got %+v", pfs)
	}
	if c, err := es.CountPendingFacts(); err != nil || c != 2 {
		t.Fatalf("count pending: c=%d err=%v, want 2", c, err)
	}

	// 引用 f1 后它自动离队——纯视图，零簿记。
	if err := es.Link(f1, RelDerivedFrom, it); err != nil {
		t.Fatal(err)
	}
	pfs, err = es.PendingFacts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pfs) != 1 || pfs[0].ID != f2 {
		t.Fatalf("after citing f1 pending want [f2], got %+v", pfs)
	}
	if c, _ := es.CountPendingFacts(); c != 1 {
		t.Fatalf("count after cite: %d, want 1", c)
	}
}

func TestDeprecateIntentQueries(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "deprecate queries")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// 两条 open 意图：i1 走完整废弃流（原因→CAS），i2 只停不写原因（模拟历史 stopped）。
	i1, err := es.AddIntent(map[string]any{"summary": "方向A"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	i2, err := es.AddIntent(map[string]any{"summary": "方向B"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}

	// 先写原因再转状态（deprecate_intent 的调用序）。
	if err := es.SetIntentStopReason(i1, "前提已被事实#42推翻"); err != nil {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(i1, "open", "stopped"); err != nil || !ok {
		t.Fatal(err)
	}
	// i2 模拟历史/急停：仅 CAS，无原因。
	if ok, err := es.CompareAndSetIntentState(i2, "open", "stopped"); err != nil || !ok {
		t.Fatal(err)
	}

	// DeprecatedIntents：最新在前；i1 带原因、i2 原因为空（NULL→""）。
	deps, err := es.DeprecatedIntents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 || deps[0].ID != i2 || deps[1].ID != i1 {
		t.Fatalf("deprecated want newest-first [i2,i1], got %+v", deps)
	}
	if deps[1].StopReason != "前提已被事实#42推翻" {
		t.Fatalf("i1 stop_reason missing: %+v", deps[1])
	}
	if deps[0].StopReason != "" {
		t.Fatalf("legacy stopped should have empty StopReason: %+v", deps[0])
	}
	if c, err := es.CountDeprecatedIntents(); err != nil || c != 2 {
		t.Fatalf("count deprecated: c=%d err=%v, want 2", c, err)
	}

	// 原因可幂等覆盖（重废弃/修正措辞）。
	if err := es.SetIntentStopReason(i1, "前提已被事实#42推翻（复核确认）"); err != nil {
		t.Fatal(err)
	}
	n, err := es.GetNode(i1)
	if err != nil || n == nil || n.StopReason != "前提已被事实#42推翻（复核确认）" {
		t.Fatalf("stop_reason overwrite failed: %+v err=%v", n, err)
	}

	// CAS 竞态：open 意图被领走（open→running）后，open→stopped 必须失败。
	i3, err := es.AddIntent(map[string]any{"summary": "方向C"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(i3, "open", "running"); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(i3, "open", "stopped"); err == nil && ok {
		t.Fatalf("CAS open->stopped on a running intent must fail")
	}
	// running→stopped 仍通（引擎 settle 路径）。
	if err := es.SetIntentStopReason(i3, "跑偏止损"); err != nil {
		t.Fatal(err)
	}
	if ok, err := es.CompareAndSetIntentState(i3, "running", "stopped"); err != nil || !ok {
		t.Fatal(err)
	}
	if c, _ := es.CountDeprecatedIntents(); c != 3 {
		t.Fatalf("count after i3: %d, want 3", c)
	}
}
