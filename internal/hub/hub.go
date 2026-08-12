// Package hub fans provider updates out to the TUI and the web UI, owns the
// fixed set of display slots, and holds each agent's event ring buffer.
package hub

import (
	"log"
	"sync"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

// FrameType tags what a Frame carries.
type FrameType string

const (
	FrameSnapshot FrameType = "snapshot" // full state, sent once on subscribe
	FrameAgent    FrameType = "agent"    // agent metadata changed
	FrameEvents   FrameType = "events"   // new stream events
	FrameSlots    FrameType = "slots"    // slot assignment changed
)

// Frame is one message to a subscriber.
type Frame struct {
	Type    FrameType                `json:"type"`
	Agents  []model.Agent            `json:"agents,omitempty"`
	Agent   *model.Agent             `json:"agent,omitempty"`
	Events  []model.Event            `json:"events,omitempty"`
	Slots   []string                 `json:"slots,omitempty"`
	Pending []string                 `json:"pending,omitempty"`
	Buffers map[string][]model.Event `json:"buffers,omitempty"`
}

type state struct {
	agent  model.Agent
	ring   []model.Event
	bytes  int64
	warned bool // buffer-wrap warning already emitted
}

// Hub is the single source of truth for live agent state.
type Hub struct {
	mu       sync.Mutex
	slots    []string // slot index -> agent id ("" when free)
	pending  []string // agents waiting for a slot; never bumps a live agent
	agents   map[string]*state
	order    []string // discovery order, oldest first
	seq      uint64
	subs     map[int]chan Frame
	nextSub  int
	maxBytes int64
}

// New returns a hub with the given number of display slots and per-agent ring
// buffer budget in bytes.
func New(slots int, maxBytes int64) *Hub {
	return &Hub{
		slots:    make([]string, slots),
		agents:   map[string]*state{},
		subs:     map[int]chan Frame{},
		maxBytes: maxBytes,
	}
}

// Apply merges one provider update into hub state and broadcasts the deltas.
func (h *Hub) Apply(u provider.Update) {
	h.mu.Lock()

	id := u.Agent.ID
	st, existed := h.agents[id]
	wasLive, wasBacklog := false, false
	if !existed {
		st = &state{}
		h.agents[id] = st
		h.order = append(h.order, id)
	} else {
		wasLive, wasBacklog = st.agent.Status == model.StatusLive, st.agent.Backlog
	}

	// Provider owns descriptive fields; hub owns buffer accounting.
	dropped, bytes := st.agent.Dropped, st.agent.Bytes
	st.agent = u.Agent
	st.agent.Dropped, st.agent.Bytes = dropped, bytes
	h.linkParents(id)

	var fresh []model.Event
	for _, e := range u.Events {
		h.seq++
		e.Seq = h.seq
		e.AgentID = id
		if e.TS.IsZero() {
			e.TS = time.Now()
		}
		st.ring = append(st.ring, e)
		st.bytes += e.Size()
		fresh = append(fresh, e)
	}
	h.trim(st)
	st.agent.Bytes = st.bytes

	slotsChanged := false
	resumed := existed && (!wasLive || wasBacklog) && st.agent.Status == model.StatusLive && !st.agent.Backlog
	if !existed || resumed {
		slotsChanged = h.assign(id)
	}
	if (st.agent.Status != model.StatusLive || st.agent.Backlog) && h.removePending(id) {
		slotsChanged = true
	}
	if st.agent.Status != model.StatusLive {
		if h.promote() {
			slotsChanged = true
		}
	}

	agent := st.agent
	slots := append([]string(nil), h.slots...)
	pendingCopy := append([]string(nil), h.pending...)
	h.mu.Unlock()

	h.broadcast(Frame{Type: FrameAgent, Agent: &agent})
	if len(fresh) > 0 {
		h.broadcast(Frame{Type: FrameEvents, Events: fresh})
	}
	if slotsChanged {
		h.broadcast(Frame{Type: FrameSlots, Slots: slots, Pending: pendingCopy})
	}
}

// trim enforces the ring buffer budget, dropping oldest events first. Hitting
// this is not expected in practice — measured transcripts are far smaller — so
// it is reported loudly rather than silently.
func (h *Hub) trim(st *state) {
	if h.maxBytes <= 0 {
		return
	}
	n := 0
	for st.bytes > h.maxBytes && len(st.ring) > 1 {
		// Drop the oldest real event, never the notice that records the wrap —
		// otherwise the warning is the first thing to disappear.
		i := 0
		for i < len(st.ring) && st.ring[i].Kind == model.EvNotice {
			i++
		}
		if i >= len(st.ring) {
			break
		}
		st.bytes -= st.ring[i].Size()
		st.ring = append(st.ring[:i], st.ring[i+1:]...)
		n++
	}
	if n == 0 {
		return
	}
	st.agent.Dropped += n
	if !st.warned {
		st.warned = true
		log.Printf("ERROR: ring buffer full for agent %s (%s) — budget %d bytes exceeded, dropping oldest events; raise buffer_bytes",
			st.agent.ID, st.agent.Label(), h.maxBytes)
		h.seq++
		notice := model.Event{
			Seq: h.seq, AgentID: st.agent.ID, TS: time.Now(),
			Kind: model.EvNotice, Err: true,
			Header: "ring buffer wrapped — oldest events dropped (raise buffer_bytes)",
		}
		st.ring = append(st.ring, notice)
		st.bytes += notice.Size()
	}
}

// assign places an agent into a slot. Free slots first, then the
// least-recently-updated *finished* agent's slot. A live agent is never bumped;
// if all slots are live the newcomer waits in the pending strip.
func (h *Hub) assign(id string) bool {
	if st := h.agents[id]; st != nil && st.agent.Backlog {
		return false // history only: never takes a pane
	}
	for _, s := range h.slots {
		if s == id {
			return false
		}
	}
	for i, s := range h.slots {
		if s == "" {
			h.slots[i] = id
			return true
		}
	}
	victim, oldest := -1, time.Time{}
	for i, s := range h.slots {
		st := h.agents[s]
		if st == nil {
			victim = i
			break
		}
		if st.agent.Status == model.StatusLive {
			continue
		}
		if victim < 0 || st.agent.Updated.Before(oldest) {
			victim, oldest = i, st.agent.Updated
		}
	}
	if victim >= 0 {
		h.slots[victim] = id
		return true
	}
	for _, p := range h.pending {
		if p == id {
			return false
		}
	}
	h.pending = append(h.pending, id)
	return true
}

func (h *Hub) removePending(id string) bool {
	kept := h.pending[:0]
	for _, pendingID := range h.pending {
		if pendingID != id {
			kept = append(kept, pendingID)
		}
	}
	changed := len(kept) != len(h.pending)
	h.pending = kept
	return changed
}

// promote moves pending agents into slots freed by finished ones.
func (h *Hub) promote() bool {
	changed := false
	for len(h.pending) > 0 {
		next := h.pending[0]
		placed := false
		for i, s := range h.slots {
			st := h.agents[s]
			if s == "" || (st != nil && st.agent.Status != model.StatusLive) {
				h.slots[i] = next
				placed, changed = true, true
				break
			}
		}
		if !placed {
			break
		}
		h.pending = h.pending[1:]
	}
	return changed
}

// Snapshot returns the current full state, used for new subscribers and for the
// TUI's initial paint.
func (h *Hub) Snapshot() Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked()
}

func (h *Hub) snapshotLocked() Frame {
	f := Frame{
		Type:    FrameSnapshot,
		Slots:   append([]string(nil), h.slots...),
		Pending: append([]string(nil), h.pending...),
		Buffers: map[string][]model.Event{},
	}
	for _, id := range h.order {
		st := h.agents[id]
		f.Agents = append(f.Agents, st.agent)
	}
	for _, id := range h.slots {
		if st := h.agents[id]; st != nil {
			f.Buffers[id] = append([]model.Event(nil), st.ring...)
		}
	}
	return f
}

// Events returns a copy of one agent's buffered events.
func (h *Hub) Events(id string) []model.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.agents[id]
	if st == nil {
		return nil
	}
	return append([]model.Event(nil), st.ring...)
}

// Agent returns a copy of one agent's metadata.
func (h *Hub) Agent(id string) (model.Agent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.agents[id]
	if st == nil {
		return model.Agent{}, false
	}
	return st.agent, true
}

// linkParents names the spawning agent on every agent whose parent hivewire
// also tracks. It runs both directions because a nested agent can be discovered
// before or after the agent that spawned it.
func (h *Hub) linkParents(changed string) {
	labels := make(map[string]string, len(h.agents))
	for _, st := range h.agents {
		if st.agent.NativeID != "" {
			labels[st.agent.Provider+":"+st.agent.NativeID] = st.agent.Label()
		}
	}
	for id, st := range h.agents {
		if st.agent.Parent == "" {
			continue
		}
		if id != changed && st.agent.ParentLabel != "" {
			continue
		}
		st.agent.ParentLabel = labels[st.agent.Provider+":"+st.agent.Parent]
	}
}

// Agents returns all agents seen this run, oldest first.
func (h *Hub) Agents() []model.Agent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]model.Agent, 0, len(h.order))
	for _, id := range h.order {
		out = append(out, h.agents[id].agent)
	}
	return out
}

// Focus forces an agent into a specific slot, swapping with whatever is there.
func (h *Hub) Focus(slot int, id string) {
	h.mu.Lock()
	if slot < 0 || slot >= len(h.slots) {
		h.mu.Unlock()
		return
	}
	for i, s := range h.slots {
		if s == id {
			h.slots[i] = h.slots[slot]
		}
	}
	h.slots[slot] = id
	h.removePending(id)
	slots := append([]string(nil), h.slots...)
	pending := append([]string(nil), h.pending...)
	h.mu.Unlock()
	h.broadcast(Frame{Type: FrameSlots, Slots: slots, Pending: pending})
}

// Subscribe registers a listener and returns its channel plus an unsubscribe
// function. The first frame delivered is always a snapshot.
func (h *Hub) Subscribe(buffer int) (<-chan Frame, func()) {
	h.mu.Lock()
	id := h.nextSub
	h.nextSub++
	ch := make(chan Frame, buffer)
	h.subs[id] = ch
	snap := h.snapshotLocked()
	h.mu.Unlock()

	ch <- snap
	return ch, func() {
		h.mu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.mu.Unlock()
	}
}

// broadcast delivers a frame to every subscriber, skipping any whose buffer is
// full so one stalled client cannot block the pipeline.
func (h *Hub) broadcast(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- f:
		default:
		}
	}
}
