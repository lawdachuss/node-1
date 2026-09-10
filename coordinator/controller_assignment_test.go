package coordinator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teacat/chaturbate-dvr/database"
)

func TestEqualSplitTargetHandlesMoreNodesThanChannels(t *testing.T) {
	nodes := make([]database.Node, 18)
	for i := range nodes {
		nodes[i].NodeID = fmt.Sprintf("node-%02d", i+1)
	}

	counts := make(map[string]int)
	for i := 0; i < 19; i++ {
		counts[equalSplitTarget(i, 19, nodes)]++
	}
	for _, node := range nodes {
		want := 1
		if node.NodeID == "node-01" {
			want = 2
		}
		if got := counts[node.NodeID]; got != want {
			t.Fatalf("%s received %d channels, want %d", node.NodeID, got, want)
		}
	}
}

func TestRecordingLeaseFreshness(t *testing.T) {
	now := time.Now().UTC()
	if !recordingLeaseFresh(now.Add(-recordingLeaseTTL+time.Second).Format(time.RFC3339Nano), now) {
		t.Fatal("fresh recording lease considered stale")
	}
	if recordingLeaseFresh(now.Add(-recordingLeaseTTL-time.Second).Format(time.RFC3339Nano), now) {
		t.Fatal("expired recording lease considered fresh")
	}
	if recordingLeaseFresh("not-a-time", now) {
		t.Fatal("invalid recording lease considered fresh")
	}
}

// testNode constructs a Node helper for balance/hasMovableImbalance tests.
func testNode(id string, deadline *time.Time) database.Node {
	return database.Node{NodeID: id, Status: "online", SessionDeadline: deadline}
}

// TestHasMovableImbalanceIgnoresPinnedRecordingOnMigratingNode verifies that
// a deadline-migrating node's in-progress recording does not by itself create
// an imbalance: recordings count toward their owner's share, so equal counts
// stay balanced no matter which rows are recordings — while a movable
// over-share row still triggers a sweep.
func TestHasMovableImbalanceIgnoresPinnedRecordingOnMigratingNode(t *testing.T) {
	active := []database.Node{testNode("node-a", nil), testNode("node-b", nil)}
	activeSet := map[string]bool{"node-a": true, "node-b": true}
	heldSet := map[string]bool{}
	protected := map[string]bool{"node-a": true, "node-b": true}

	c := &Coordinator{}

	// a: 1 claimed + 1 recording; b: 2 claimed → counts 2 vs 2 → balanced;
	// the recording changes nothing (it occupies one of node-a's slots).
	balanced := []database.ChannelAssignment{
		{Username: "aRec", AssignedNode: "node-a", Status: "recording"},
		{Username: "a1", AssignedNode: "node-a", Status: "claimed"},
		{Username: "b1", AssignedNode: "node-b", Status: "claimed"},
		{Username: "b2", AssignedNode: "node-b", Status: "claimed"},
	}
	if c.hasMovableImbalance(balanced, active, activeSet, heldSet, protected) {
		t.Fatal("2 vs 2 counts are balanced — a pinned recording must not create an imbalance")
	}

	// a: 1 recording + 3 claimed; b: 1 claimed → total 5, targets 3/2.
	// node-a (4) is over its share AND owns movable rows → sweep warranted.
	movableOver := []database.ChannelAssignment{
		{Username: "aRec", AssignedNode: "node-a", Status: "recording"},
		{Username: "a1", AssignedNode: "node-a", Status: "claimed"},
		{Username: "a2", AssignedNode: "node-a", Status: "claimed"},
		{Username: "a3", AssignedNode: "node-a", Status: "claimed"},
		{Username: "b1", AssignedNode: "node-b", Status: "claimed"},
	}
	if !c.hasMovableImbalance(movableOver, active, activeSet, heldSet, protected) {
		t.Fatal("movable over-share row should trigger a sweep")
	}
}

// TestHasMovableImbalanceSeesImbalanceWhenProtectedNodeHasNoMovableWork
// guarantees that dropping a genuine movable channel out of a protected set
// surfaces an imbalance (i.e., we did not permanently pin everything).
func TestHasMovableImbalanceDetectsRealImbalance(t *testing.T) {
	active := []database.Node{testNode("node-a", nil), testNode("node-b", nil)}
	activeSet := map[string]bool{"node-a": true, "node-b": true}
	heldSet := map[string]bool{}
	protected := map[string]bool{} // neither recording → both fully movable

	all := []database.ChannelAssignment{
		{Username: "idle1", AssignedNode: "node-b", Status: "claimed"},
		{Username: "idle2", AssignedNode: "node-b", Status: "claimed"},
		{Username: "idle3", AssignedNode: "node-a", Status: "claimed"},
	}
	c := &Coordinator{}
	if !c.hasMovableImbalance(all, active, activeSet, heldSet, protected) {
		t.Fatal("imbalance (2 vs 1) should be detected when work is movable")
	}
}

func TestPickLiveRebalanceMoveNeverMovesARecording(t *testing.T) {
	active := map[string]bool{"node-a": true, "node-b": true}
	// node-a is over share (2 rec vs fair 1). It owns a live-claimed channel
	// (movable) AND an in-progress recording. The move must pick the live-claimed
	// channel and NEVER the recording.
	all := []database.ChannelAssignment{
		{Username: "aLiveMove", AssignedNode: "node-a", Status: "claimed", IsLive: true},
		{Username: "aRec", AssignedNode: "node-a", Status: "recording", IsLive: true},
	}
	rec := map[string]int{"node-a": 2, "node-b": 0}
	m := pickLiveRebalanceMove(all, active, rec, 1)
	if m == nil {
		t.Fatal("expected a move: node-a is over share and owns a live-claimed channel")
	}
	if m.ca.Username != "aLiveMove" {
		t.Fatalf("pickLiveRebalanceMove picked %q, want the live-claimed channel (never a recording)", m.ca.Username)
	}
	if m.ca.Status == "recording" {
		t.Fatalf("pickLiveRebalanceMove returned a recording channel: %+v", m.ca)
	}
}

func TestPickLiveRebalanceMoveRequiresLiveNotRecordingCandidate(t *testing.T) {
	active := map[string]bool{"node-a": true, "node-b": true}
	rec := map[string]int{"node-a": 3, "node-b": 0}
	// Node-a is heavily over share (fair 2), but the over-share channel it owns
	// is OFFLINE (not live) — nothing is live-and-not-recording, so no move.
	all := []database.ChannelAssignment{
		{Username: "a1", AssignedNode: "node-a", Status: "claimed", IsLive: false},
	}
	if m := pickLiveRebalanceMove(all, active, rec, 2); m != nil {
		t.Fatalf("no live-and-not-recording channel should be moved, got %+v", m.ca)
	}
}

func TestPickLiveRebalanceMoveRelievesOverShareNodeToColdest(t *testing.T) {
	active := map[string]bool{"node-a": true, "node-b": true, "node-c": true}
	rec := map[string]int{"node-a": 3, "node-b": 1, "node-c": 0}
	all := []database.ChannelAssignment{
		// node-a is over share (3 vs fair 2); node-c is coldest (0).
		{Username: "aLive", AssignedNode: "node-a", Status: "claimed", IsLive: true},
		// A recording on node-a must remain untouched.
		{Username: "aRec", AssignedNode: "node-a", Status: "recording", IsLive: true},
	}
	m := pickLiveRebalanceMove(all, active, rec, 2)
	if m == nil {
		t.Fatal("expected a move: node-a is over share and owns a live claimed channel")
	}
	if m.src != "node-a" {
		t.Fatalf("source = %s, want node-a", m.src)
	}
	if m.dst != "node-c" {
		t.Fatalf("dst = %s, want node-c (coldest)", m.dst)
	}
	if m.ca.Status == "recording" {
		t.Fatal("must never move a recording")
	}
}

func TestPickLiveRebalanceMoveTieBreaksDestByNodeID(t *testing.T) {
	active := map[string]bool{"node-a": true, "node-b": true, "node-c": true}
	rec := map[string]int{"node-a": 3, "node-b": 0, "node-c": 0}
	all := []database.ChannelAssignment{
		{Username: "aLive", AssignedNode: "node-a", Status: "claimed", IsLive: true},
	}
	// node-b and node-c both have 0 recordings (tie) and both below fair 2;
	// node-b sorts first.
	m := pickLiveRebalanceMove(all, active, rec, 2)
	if m == nil || m.dst != "node-b" {
		t.Fatalf("dst = %v, want node-b (tie-break by node_id)", m)
	}
}

func TestPickLiveRebalanceMoveNilWhenBalanced(t *testing.T) {
	active := map[string]bool{"node-a": true, "node-b": true}
	rec := map[string]int{"node-a": 1, "node-b": 1}
	all := []database.ChannelAssignment{
		{Username: "aLive", AssignedNode: "node-a", Status: "claimed", IsLive: true},
	}
	if m := pickLiveRebalanceMove(all, active, rec, 1); m != nil {
		t.Fatalf("node-a at fair share should not shed, got %+v", m.ca)
	}
}

func TestPickLiveRebalanceMoveSkipsUnassignedAndNonActiveNodes(t *testing.T) {
	active := map[string]bool{"node-b": true}
	rec := map[string]int{"node-b": 0}
	all := []database.ChannelAssignment{
		{Username: "orphan", AssignedNode: "", Status: "claimed", IsLive: true},
		{Username: "deadOwner", AssignedNode: "node-x", Status: "claimed", IsLive: true},
	}
	if m := pickLiveRebalanceMove(all, active, rec, 1); m != nil {
		t.Fatalf("channels on unassigned/off-node must never be moved, got %+v", m.ca)
	}
}

// TestHasMovableImbalanceStableWhenRecordingsRun pins the 2026-09-10 fleet
// regression: a node that is over the equal split ONLY because of pinned
// recordings (which can never be moved) must not keep the imbalance check
// true — that made the controller re-sweep the whole pool every cycle and
// stop/start channels fleet-wide. Recordings count toward the share; only
// movable (claimed) over-share rows justify a sweep.
func TestHasMovableImbalanceStableWhenRecordingsRun(t *testing.T) {
	active := []database.Node{testNode("node-a", nil), testNode("node-b", nil)}
	activeSet := map[string]bool{"node-a": true, "node-b": true}
	heldSet := map[string]bool{}
	protected := map[string]bool{"node-a": true, "node-b": true}

	c := &Coordinator{}

	// 2 vs 2: node-a owns a recording, node-b owns a claimed channel. Counts
	// are equal → no imbalance regardless of movability.
	balanced := []database.ChannelAssignment{
		{Username: "aRec", AssignedNode: "node-a", Status: "recording"},
		{Username: "b1", AssignedNode: "node-b", Status: "claimed"},
	}
	if c.hasMovableImbalance(balanced, active, activeSet, heldSet, protected) {
		t.Fatal("2 vs 2 counts are balanced — no sweep")
	}

	// a: 1 recording + 2 claimed; b: 1 claimed → total 4, targets 2/2.
	// node-a is over its share AND owns movable rows → a sweep is warranted.
	movableOver := []database.ChannelAssignment{
		{Username: "aRec", AssignedNode: "node-a", Status: "recording"},
		{Username: "a1", AssignedNode: "node-a", Status: "claimed"},
		{Username: "a2", AssignedNode: "node-a", Status: "claimed"},
		{Username: "b1", AssignedNode: "node-b", Status: "claimed"},
	}
	if !c.hasMovableImbalance(movableOver, active, activeSet, heldSet, protected) {
		t.Fatal("movable over-share row should trigger a sweep")
	}

	// a: 3 recordings; b: 1 claimed → total 4, targets 2/2. node-a is over its
	// share but every over-share row is a pinned recording. Nothing movable →
	// the fleet must be left alone (this is the state that used to sweep forever).
	pinnedOver := []database.ChannelAssignment{
		{Username: "aRec1", AssignedNode: "node-a", Status: "recording"},
		{Username: "aRec2", AssignedNode: "node-a", Status: "recording"},
		{Username: "aRec3", AssignedNode: "node-a", Status: "recording"},
		{Username: "b1", AssignedNode: "node-b", Status: "claimed"},
	}
	if c.hasMovableImbalance(pinnedOver, active, activeSet, heldSet, protected) {
		t.Fatal("over-share from pinned recordings only must NOT trigger a sweep")
	}
}

// staleMoveDB is a stateful fake for the reassign RPC: it applies the same
// guard as reassign_channel (assigned_node = p_from_node AND status <>
// 'recording') and reports whether the row actually moved.
type staleMoveDB struct {
	assignments []database.ChannelAssignment
	posts       int
}

func (f *staleMoveDB) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && strings.Contains(r.URL.Path, "/rpc/claim_controller_lease"):
		json.NewEncoder(w).Encode(true)
	case r.Method == "POST" && strings.Contains(r.URL.Path, "/rpc/reassign_channel"):
		f.posts++
		var body struct {
			PUsername string `json:"p_username"`
			PSite     string `json:"p_site"`
			PFromNode string `json:"p_from_node"`
			PToNode   string `json:"p_to_node"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		moved := false
		for i := range f.assignments {
			ca := &f.assignments[i]
			if ca.Username == body.PUsername && ca.Site == body.PSite &&
				ca.AssignedNode == body.PFromNode && ca.Status != "recording" {
				ca.AssignedNode = body.PToNode
				ca.Status = "claimed"
				moved = true
				break
			}
		}
		json.NewEncoder(w).Encode([]bool{moved})
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func newStaleMoveClient(t *testing.T, fake *staleMoveDB) *database.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	return database.NewClient(srv.URL, "test-key")
}

// TestRebalanceLiveLoadNeverMovesTheSameRowTwice reproduces the 2026-09-10
// avrora_jessie incident: the live-load balancer re-picked the same row from
// its STALE snapshot after every successful move, overwriting each previous
// destination (node-16 -> node-1 -> node-10 -> ... six times in one tick).
// The snapshot must be updated after a real move so the row is not re-picked.
func TestRebalanceLiveLoadNeverMovesTheSameRowTwice(t *testing.T) {
	fake := &staleMoveDB{
		assignments: []database.ChannelAssignment{
			{Username: "avrora", Site: "chaturbate", AssignedNode: "node-16", Status: "claimed", IsLive: true},
			// node-16 is carrying 6 recordings — far over any fair share — so
			// without the snapshot fix the balancer keeps re-picking avrora from
			// its stale snapshot and re-moving it (6 posts, one per shed unit,
			// exactly the 2026-09-10 incident shape).
			{Username: "rec1", Site: "chaturbate", AssignedNode: "node-16", Status: "recording", IsLive: true},
			{Username: "rec2", Site: "chaturbate", AssignedNode: "node-16", Status: "recording", IsLive: true},
			{Username: "rec3", Site: "chaturbate", AssignedNode: "node-16", Status: "recording", IsLive: true},
			{Username: "rec4", Site: "chaturbate", AssignedNode: "node-16", Status: "recording", IsLive: true},
			{Username: "rec5", Site: "chaturbate", AssignedNode: "node-16", Status: "recording", IsLive: true},
			{Username: "rec6", Site: "chaturbate", AssignedNode: "node-16", Status: "recording", IsLive: true},
		},
	}
	c := &Coordinator{Client: newStaleMoveClient(t, fake)}
	active := map[string]bool{"node-1": true, "node-10": true, "node-16": true}
	all := append([]database.ChannelAssignment{}, fake.assignments...)
	renew := func() bool { return true }

	c.rebalanceLiveLoad(all, active, renew)

	if fake.posts != 1 {
		t.Fatalf("expected exactly 1 reassign POST (the only movable row), got %d", fake.posts)
	}
	if got := all[0].AssignedNode; got == "node-16" || got == "" {
		t.Fatalf("row should have moved off node-16, snapshot says %q", got)
	}
}

// TestRebalanceLiveLoadSkipsRowLostToRace covers the no-op RPC result: when
// the guarded UPDATE matches zero rows (row moved or became recording
// elsewhere), the loop must mark the row done and terminate — not spin
// forever and not count it as a move.
func TestRebalanceLiveLoadSkipsRowLostToRace(t *testing.T) {
	fake := &staleMoveDB{
		// DB reality: the row flipped to 'recording' after the controller read
		// its snapshot. The snapshot below still says claimed on node-a.
		assignments: []database.ChannelAssignment{
			{Username: "racer", Site: "chaturbate", AssignedNode: "node-a", Status: "recording", IsLive: true},
			{Username: "recX", Site: "chaturbate", AssignedNode: "node-a", Status: "recording", IsLive: true},
			{Username: "recY", Site: "chaturbate", AssignedNode: "node-a", Status: "recording", IsLive: true},
		},
	}
	c := &Coordinator{Client: newStaleMoveClient(t, fake)}
	active := map[string]bool{"node-a": true, "node-b": true}
	all := []database.ChannelAssignment{
		{Username: "racer", Site: "chaturbate", AssignedNode: "node-a", Status: "claimed", IsLive: true},
		{Username: "recX", Site: "chaturbate", AssignedNode: "node-a", Status: "recording", IsLive: true},
		{Username: "recY", Site: "chaturbate", AssignedNode: "node-a", Status: "recording", IsLive: true},
	}
	renew := func() bool { return true }

	c.rebalanceLiveLoad(all, active, renew) // must return, not hang

	if fake.posts != 1 {
		t.Fatalf("expected exactly 1 (rejected) reassign POST, got %d", fake.posts)
	}
	if got := all[0].AssignedNode; got != "node-a" {
		t.Fatalf("no-op move must not change the snapshot owner, got %q", got)
	}
	if got := all[0].Status; got != "recording" {
		t.Fatalf("no-op move must mark the row done in the snapshot, got status %q", got)
	}
}

// TestFleetSignatureIncludesMigratingNode verifies that a node entering the
// deadline-migration window produces a distinct fleet signature, so the
// controller triggers exactly one reassignment when it enters/exits.
func TestFleetSignatureIncludesMigratingNode(t *testing.T) {
	now := time.Now()
	active := []database.Node{testNode("node-a", nil), testNode("node-b", nil)}

	sigNormal := fleetSignature(active, nil, nil)
	if strings.Contains(sigNormal, "M:node-b") {
		t.Fatalf("normal signature should not mark node-b as migrating: %q", sigNormal)
	}

	migrating := []database.Node{testNode("node-b", &now)}
	sigMigrating := fleetSignature(active, migrating, nil)
	if !strings.Contains(sigMigrating, "M:node-b") {
		t.Fatalf("migrating signature should mark node-b: %q", sigMigrating)
	}
	if sigNormal == sigMigrating {
		t.Fatal("fleet signature must differ when a node enters the migration window")
	}
}
