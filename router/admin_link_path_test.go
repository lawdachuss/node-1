package router

import (
	"strings"
	"testing"

	"github.com/teacat/chaturbate-dvr/entity"
)

// TestAdminPanelShowsLinkWritePathState pins the operator-facing half of the
// upload-link write-path canary.  The 2026-09-23 incident (a trigger rejecting
// every upload_links insert) was invisible for most of a day because nothing on
// screen distinguished "this fleet cannot save a link at all" from "a few
// recordings are being sorted out" — the panel has to say so itself, and must not
// claim health before a check has run.
func TestAdminPanelShowsLinkWritePathState(t *testing.T) {
	cases := []struct {
		name    string
		recon   *RecordingReconciliation
		want    []string
		notWant []string
	}{
		{
			name: "closed write path",
			recon: &RecordingReconciliation{
				LinkWritePathChecked: true,
				LinkWritePathDetail:  "upload_links insert rejected: HTTP 404: {\"code\":\"42883\"}",
			},
			want:    []string{"Link writes (this node)", ">FAILING</div>", "recon-critical-text", "42883"},
			notWant: []string{">ok</div>"},
		},
		{
			name:    "healthy",
			recon:   &RecordingReconciliation{LinkWritePathChecked: true, LinkWritePathOK: true},
			want:    []string{"Link writes (this node)", ">ok</div>"},
			notWant: []string{"FAILING", "not checked yet"},
		},
		{
			name:    "not checked yet",
			recon:   &RecordingReconciliation{},
			want:    []string{"Link writes (this node)", "not checked yet"},
			notWant: []string{"FAILING", ">ok</div>"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderTemplate(t, "admin.html", &AdminData{
				Config:  &entity.Config{},
				Recon:   tc.recon,
				Uploads: &entity.UploadsResponse{},
				Disk:    &entity.DiskInfo{Total: "1 TB", Used: "1 GB", Free: "999 GB", Percent: 1},
			})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("admin.html is missing %q for the link write path (%s)", want, tc.name)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("admin.html rendered %q for the link write path, which is wrong for %s", notWant, tc.name)
				}
			}
		})
	}
}
