package router

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/teacat/chaturbate-dvr/server"
)

// HealthAPI reports this node's readiness as JSON, for monitors, the fleet-audit
// scripts and anything else that should not have to scrape HTML to notice a
// broken database write path.
//
// It answers two questions: is the database reachable, and is the upload_links
// write path open?  The second one is the important one — the 2026-09-23 incident
// (a trigger rejecting every insert) left reads, the schema cache and `select 1`
// perfectly green while the fleet silently failed to mark any recording as
// uploaded.  The answer comes from the cached periodic canary
// (server.CheckUploadLinkWritePath), so nothing here queries the database beyond
// the reachability probe.
//
// 200 when everything is fine, 503 otherwise, so a plain status check is enough.
func HealthAPI(c *gin.Context) {
	type linkPath struct {
		State     string `json:"state"` // ok | failing | unchecked
		Detail    string `json:"detail,omitempty"`
		CheckedAt string `json:"checked_at,omitempty"`
	}
	resp := struct {
		OK            bool     `json:"ok"`
		Supabase      string   `json:"supabase"` // "ok" or the error
		LinkWritePath linkPath `json:"link_write_path"`
	}{}

	resp.Supabase = "ok"
	if err := server.CheckSupabase(); err != nil {
		resp.Supabase = err.Error()
	}

	st := server.UploadLinkWritePathStatus()
	switch {
	case !st.Checked:
		resp.LinkWritePath.State = "unchecked"
	case st.OK:
		resp.LinkWritePath.State = "ok"
	default:
		resp.LinkWritePath.State = "failing"
	}
	resp.LinkWritePath.Detail = st.Detail
	if st.Checked {
		resp.LinkWritePath.CheckedAt = st.At.UTC().Format(time.RFC3339)
	}

	// Only a verified-open write path counts as healthy: "unchecked" means the
	// canary has not answered yet (it runs at startup and on the maintenance
	// ticker), and reporting an unproven path as healthy is precisely how the
	// incident stayed invisible.  A monitor watching this endpoint therefore sees a
	// brief not-ok window right after a node starts, which is the honest answer.
	resp.OK = resp.Supabase == "ok" && resp.LinkWritePath.State == "ok"

	code := http.StatusOK
	if !resp.OK {
		code = http.StatusServiceUnavailable
	}
	c.JSON(code, resp)
}
