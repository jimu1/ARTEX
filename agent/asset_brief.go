package agent

import (
	"log"

	"github.com/Autumn-27/artex/db"
)

// assetOriginView is the provenance attached to one anchored asset in the
// worker's system prompt. Source mirrors task_asset_links.source; IntentID is
// present only when the link carries a source_node_id (i.e. the registrar was a
// work agent). Note the table keeps one row per (task, asset) and later
// registrations overwrite earlier ones, so this reads "most recent registrar".
type assetOriginView struct {
	Source   string `json:"source"`
	IntentID int64  `json:"intent_id,omitempty"`
}

// assetView embeds the asset row and adds origin. The embedding keeps the
// existing asset JSON shape intact (id/url/technologies/...), so prompts that
// already match those keys keep working.
type assetView struct {
	*db.Asset
	Origin *assetOriginView `json:"origin,omitempty"`
}

// assetViews decorates assets with their in-task provenance for the system
// prompt's asset block. It degrades silently: a lookup error, a missing link row
// or a zero task id merely drops the origin key — it must never fail a run.
func assetViews(as *db.AssetStore, taskID int64, assets []*db.Asset) []assetView {
	if len(assets) == 0 {
		return []assetView{}
	}
	var origins map[int64]db.TaskAssetOrigin
	if as != nil && taskID > 0 {
		ids := make([]int64, 0, len(assets))
		for _, a := range assets {
			if a != nil {
				ids = append(ids, a.ID)
			}
		}
		var err error
		origins, err = as.TaskAssetOrigins(taskID, ids)
		if err != nil {
			log.Printf("[asset-origin] task %d 查询资产来源失败（已降级为不显示 origin）: %v", taskID, err)
			origins = nil
		}
	}
	return buildAssetViews(assets, origins)
}

// buildAssetViews is the pure part of assetViews: it maps already-fetched
// provenance onto assets. Split out so the rendering rules can be unit tested
// without a database.
func buildAssetViews(assets []*db.Asset, origins map[int64]db.TaskAssetOrigin) []assetView {
	out := make([]assetView, 0, len(assets))
	for _, a := range assets {
		if a == nil {
			continue
		}
		view := assetView{Asset: a}
		if o, ok := origins[a.ID]; ok && (o.Source != "" || o.NodeID > 0) {
			view.Origin = &assetOriginView{Source: o.Source}
			if o.NodeID > 0 {
				view.Origin.IntentID = o.NodeID
			}
		}
		out = append(out, view)
	}
	return out
}
