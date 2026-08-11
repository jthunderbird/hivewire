package hub

import (
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

func agent(id string, status model.Status, updated time.Time) provider.Update {
	return provider.Update{Agent: model.Agent{
		ID: id, Provider: "claude", Status: status, Updated: updated,
	}}
}

func TestAgentsFillFreeSlotsInOrder(t *testing.T) {
	h := New(4, 1<<20)
	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		h.Apply(agent(id, model.StatusLive, now))
	}
	if got := h.Snapshot().Slots; got[0] != "a" || got[1] != "b" || got[2] != "c" || got[3] != "" {
		t.Fatalf("slots = %v", got)
	}
}

func TestFinishedAgentsKeepTheirPaneUntilSomeoneNeedsIt(t *testing.T) {
	h := New(2, 1<<20)
	now := time.Now()
	h.Apply(agent("old", model.StatusLive, now.Add(-time.Minute)))
	h.Apply(agent("new", model.StatusLive, now))

	// Finishing must not free the pane on its own.
	h.Apply(agent("old", model.StatusDone, now.Add(-time.Minute)))
	if got := h.Snapshot().Slots; got[0] != "old" {
		t.Fatalf("a finished agent should keep its slot, slots = %v", got)
	}

	// The next arrival recycles the oldest finished pane.
	h.Apply(agent("third", model.StatusLive, now))
	if got := h.Snapshot().Slots; got[0] != "third" || got[1] != "new" {
		t.Fatalf("newcomer should take the finished pane, slots = %v", got)
	}
}

func TestLiveAgentsAreNeverEvicted(t *testing.T) {
	h := New(2, 1<<20)
	now := time.Now()
	h.Apply(agent("a", model.StatusLive, now))
	h.Apply(agent("b", model.StatusLive, now))
	h.Apply(agent("c", model.StatusLive, now))

	snap := h.Snapshot()
	if snap.Slots[0] != "a" || snap.Slots[1] != "b" {
		t.Fatalf("live agents were bumped: %v", snap.Slots)
	}
	if len(snap.Pending) != 1 || snap.Pending[0] != "c" {
		t.Fatalf("newcomer should wait, pending = %v", snap.Pending)
	}

	// Freeing a pane promotes the waiter.
	h.Apply(agent("a", model.StatusDone, now))
	snap = h.Snapshot()
	if snap.Slots[0] != "c" || len(snap.Pending) != 0 {
		t.Fatalf("pending agent should claim the freed slot: slots=%v pending=%v", snap.Slots, snap.Pending)
	}
}

func TestBacklogAgentsAreIndexedButNeverShown(t *testing.T) {
	h := New(4, 1<<20)
	u := agent("history", model.StatusDone, time.Now())
	u.Agent.Backlog = true
	h.Apply(u)

	snap := h.Snapshot()
	for _, s := range snap.Slots {
		if s == "history" {
			t.Fatal("a backlog agent must not occupy a slot")
		}
	}
	if len(snap.Agents) != 1 {
		t.Fatal("a backlog agent should still be listed for history")
	}
}

func TestEvictedFinishedAgentRegainsSlotWhenResumed(t *testing.T) {
	h := New(1, 1<<20)
	now := time.Now()
	h.Apply(agent("old", model.StatusLive, now.Add(-time.Minute)))
	h.Apply(agent("old", model.StatusDone, now.Add(-time.Minute)))
	h.Apply(agent("replacement", model.StatusLive, now))

	h.Apply(agent("old", model.StatusLive, now.Add(time.Minute)))
	h.Apply(agent("old", model.StatusLive, now.Add(time.Minute)))
	snap := h.Snapshot()
	if snap.Slots[0] != "replacement" {
		t.Fatalf("resumed agent evicted live replacement: slots = %v", snap.Slots)
	}
	if len(snap.Pending) != 1 || snap.Pending[0] != "old" {
		t.Fatalf("resumed agent should wait once, pending = %v", snap.Pending)
	}

	h.Apply(agent("replacement", model.StatusDone, now.Add(2*time.Minute)))
	snap = h.Snapshot()
	if snap.Slots[0] != "old" || len(snap.Pending) != 0 {
		t.Fatalf("resumed agent should claim finished replacement's slot: slots=%v pending=%v", snap.Slots, snap.Pending)
	}
}

func TestResumedBacklogAgentCanTakeSlot(t *testing.T) {
	h := New(1, 1<<20)
	now := time.Now()
	u := agent("history", model.StatusDone, now)
	u.Agent.Backlog = true
	h.Apply(u)

	h.Apply(agent("history", model.StatusLive, now.Add(time.Minute)))
	snap := h.Snapshot()
	if snap.Slots[0] != "history" {
		t.Fatalf("resumed backlog agent should take free slot: slots = %v", snap.Slots)
	}
}

func TestIneligiblePendingAgentIsRemoved(t *testing.T) {
	tests := []struct {
		name    string
		status  model.Status
		backlog bool
	}{
		{name: "done", status: model.StatusDone},
		{name: "error", status: model.StatusError},
		{name: "backlog", status: model.StatusLive, backlog: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(1, 1<<20)
			now := time.Now()
			h.Apply(agent("occupant", model.StatusLive, now))
			h.Apply(agent("stale", model.StatusLive, now))
			h.Apply(agent("next", model.StatusLive, now))

			u := agent("stale", tt.status, now.Add(time.Minute))
			u.Agent.Backlog = tt.backlog
			h.Apply(u)
			if got := h.Snapshot().Pending; len(got) != 1 || got[0] != "next" {
				t.Fatalf("ineligible agent should be removed without reordering waiters: pending = %v", got)
			}

			h.Apply(agent("occupant", model.StatusDone, now.Add(2*time.Minute)))
			snap := h.Snapshot()
			if snap.Slots[0] != "next" || len(snap.Pending) != 0 {
				t.Fatalf("next eligible waiter should promote: slots=%v pending=%v", snap.Slots, snap.Pending)
			}
		})
	}
}

func TestFocusRemovesAgentFromPending(t *testing.T) {
	h := New(2, 1<<20)
	now := time.Now()
	for _, id := range []string{"a", "b", "focused", "next"} {
		h.Apply(agent(id, model.StatusLive, now))
	}

	h.Focus(0, "focused")
	snap := h.Snapshot()
	if snap.Slots[0] != "focused" || snap.Slots[1] != "b" {
		t.Fatalf("focused agent should occupy requested slot: slots = %v", snap.Slots)
	}
	if len(snap.Pending) != 1 || snap.Pending[0] != "next" {
		t.Fatalf("focused agent should leave pending without reordering waiters: pending = %v", snap.Pending)
	}

	h.Apply(agent("b", model.StatusDone, now.Add(time.Minute)))
	snap = h.Snapshot()
	if snap.Slots[0] != "focused" || snap.Slots[1] != "next" || len(snap.Pending) != 0 {
		t.Fatalf("next waiter should promote without duplicating focused agent: slots=%v pending=%v", snap.Slots, snap.Pending)
	}
}

func TestRingBufferDropsOldestAndSaysSo(t *testing.T) {
	h := New(1, 512) // tiny budget so the buffer wraps immediately
	big := string(make([]byte, 300))
	for i := 0; i < 5; i++ {
		h.Apply(provider.Update{
			Agent:  model.Agent{ID: "a", Provider: "claude", Status: model.StatusLive},
			Events: []model.Event{{Kind: model.EvText, Body: big}},
		})
	}
	a, ok := h.Agent("a")
	if !ok {
		t.Fatal("agent missing")
	}
	if a.Dropped == 0 {
		t.Fatal("exceeding the budget should drop events and record it")
	}
	var noticed bool
	for _, e := range h.Events("a") {
		if e.Kind == model.EvNotice {
			noticed = true
		}
	}
	if !noticed {
		t.Fatal("a wrapped buffer must leave a visible notice in the stream")
	}
}

func TestSubscribersGetASnapshotFirst(t *testing.T) {
	h := New(4, 1<<20)
	h.Apply(agent("a", model.StatusLive, time.Now()))

	ch, cancel := h.Subscribe(4)
	defer cancel()

	select {
	case f := <-ch:
		if f.Type != FrameSnapshot || len(f.Agents) != 1 {
			t.Fatalf("first frame = %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot delivered")
	}
}
