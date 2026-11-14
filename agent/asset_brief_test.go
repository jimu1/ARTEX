package agent

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// The asset block in the worker's system prompt must show where each asset came
// from, without changing the asset JSON shape the model already knows.
func TestBuildAssetViewsAddsOrigin(t *testing.T) {
	assets := []*db.Asset{
		{ID: 138199, Type: "service", URL: "https://hrtip.example.test/fxweb/swagger-resources"},
		{ID: 137957, Type: "subdomain", Domain: "hrtip.example.test"},
		{ID: 42, Type: "endpoint", URL: "https://hrtip.example.test/y"},
	}
	origins := map[int64]db.TaskAssetOrigin{
		138199: {Source: "agent", NodeID: 20287},
		137957: {Source: "task"},
	}

	raw, err := json.Marshal(buildAssetViews(assets, origins))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 assets, got %d: %s", len(got), raw)
	}

	first, _ := got[0]["origin"].(map[string]any)
	if first == nil || first["source"] != "agent" || first["intent_id"] != float64(20287) {
		t.Fatalf("agent origin mismatch: %v", got[0]["origin"])
	}
	// The asset's own fields must survive the wrapper.
	if got[0]["url"] != "https://hrtip.example.test/fxweb/swagger-resources" {
		t.Fatalf("asset fields lost: %v", got[0])
	}

	second, _ := got[1]["origin"].(map[string]any)
	if second == nil || second["source"] != "task" {
		t.Fatalf("task origin mismatch: %v", got[1]["origin"])
	}
	if _, exists := second["intent_id"]; exists {
		t.Fatalf("task-sourced origin must not carry intent_id: %v", second)
	}

	if _, exists := got[2]["origin"]; exists {
		t.Fatalf("asset without a link row must not render origin: %v", got[2])
	}
}

func TestAssetViewsSkipsOriginWithoutStore(t *testing.T) {
	views := assetViews(nil, 0, []*db.Asset{{ID: 1, Type: "service"}})
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	if views[0].Origin != nil {
		t.Fatalf("no store/task context must not produce origin: %+v", views[0].Origin)
	}
}

func TestAssetViewsEmptyInput(t *testing.T) {
	if got := assetViews(nil, 1, nil); len(got) != 0 {
		t.Fatalf("want empty result, got %d", len(got))
	}
	// nil entries are skipped rather than crashing the prompt build.
	got := buildAssetViews([]*db.Asset{nil, {ID: 7, Type: "service"}}, nil)
	if len(got) != 1 || got[0].ID != 7 {
		t.Fatalf("nil asset should be skipped: %+v", got)
	}
}
