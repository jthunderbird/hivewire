package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

type emittedPart struct {
	fingerprint             string
	user                    bool
	textDone, reasoningDone bool
	toolUse, toolResult     bool
	malformed               string
	assistantError          string
}

type normalizedSession struct {
	agent      model.Agent
	events     []model.Event
	eventCount int
	state      map[string]emittedPart
	status     model.Status
	statusTime time.Time
}

type normalizationStats struct {
	eventGroups int
	toolBodies  int
}

type messageData struct {
	Role    string          `json:"role"`
	Agent   string          `json:"agent"`
	ModelID string          `json:"modelID"`
	Finish  string          `json:"finish"`
	Error   json.RawMessage `json:"error"`
	Model   struct {
		ModelID string `json:"modelID"`
	} `json:"model"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type partData struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	Time      struct {
		Start int64  `json:"start"`
		End   *int64 `json:"end"`
	} `json:"time"`
	State struct {
		Status string         `json:"status"`
		Input  map[string]any `json:"input"`
		Output string         `json:"output"`
		Error  string         `json:"error"`
		Time   struct {
			Start int64  `json:"start"`
			End   *int64 `json:"end"`
		} `json:"time"`
	} `json:"state"`
}

type partHeader struct {
	Type string `json:"type"`
}

type eventGroup struct {
	effective int64
	created   int64
	rowID     string
	events    []model.Event
}

func normalizeSession(session sessionRow, sessions []sessionRow, messages []messageRow, parts []partRow, source string, prior map[string]emittedPart) (normalizedSession, error) {
	return normalizeSessionMode(session, sessions, messages, parts, source, prior, true, nil)
}

func normalizeSessionMode(session sessionRow, sessions []sessionRow, messages []messageRow, parts []partRow, source string, prior map[string]emittedPart, emitEvents bool, stats *normalizationStats) (normalizedSession, error) {
	depth, err := sessionDepth(session, sessions)
	if err != nil {
		return normalizedSession{}, err
	}

	result := normalizedSession{state: make(map[string]emittedPart), status: model.StatusLive}
	result.agent = model.Agent{
		ID:         "opencode:" + session.id,
		NativeID:   session.id,
		Provider:   "opencode",
		Name:       session.agent,
		Title:      session.title,
		Depth:      depth,
		Parent:     session.parentID,
		Cwd:        session.directory,
		Model:      session.model,
		CLIVersion: session.version,
		Source:     source,
		Started:    unixMillis(session.timeCreated),
		Updated:    unixMillis(session.timeUpdated),
		Status:     model.StatusLive,
		Tokens: model.Tokens{
			In:         int(session.tokensInput),
			Out:        int(session.tokensOutput),
			Reasoning:  int(session.tokensReasoning),
			CacheRead:  int(session.tokensCacheRead),
			CacheWrite: int(session.tokensCacheWrite),
			Total:      int(session.tokensInput + session.tokensOutput),
		},
	}

	messageByID := make(map[string]messageData, len(messages))
	var groups []eventGroup
	var newest *messageRow
	var newestData messageData
	var newestMalformed bool
	for i := range messages {
		row := messages[i]
		result.agent.Updated = laterTime(result.agent.Updated, unixMillis(row.timeUpdated))
		authoritative := newest == nil || row.timeCreated > newest.timeCreated || row.timeCreated == newest.timeCreated && row.id > newest.id
		if authoritative {
			newest = &messages[i]
			newestData = messageData{}
			newestMalformed = false
		}
		fingerprint := rowFingerprint(row.data)
		state := prior[row.id]
		if state.fingerprint != fingerprint {
			state.malformed = ""
			state.assistantError = ""
		}
		state.fingerprint = fingerprint

		var data messageData
		if err := json.Unmarshal([]byte(row.data), &data); err != nil {
			if authoritative {
				newestMalformed = true
			}
			if state.malformed != fingerprint {
				result.eventCount++
				if emitEvents {
					notice := malformedNotice(result.agent.ID, "message", row.id, err, row.timeUpdated, row.timeCreated)
					groups = append(groups, eventGroup{effective: eventMillis(notice.TS), created: row.timeCreated, rowID: row.id, events: []model.Event{notice}})
					recordEventGroup(stats)
				}
				state.malformed = fingerprint
			}
			result.state[row.id] = state
			continue
		}
		messageByID[row.id] = data
		if result.agent.Name == "" && data.Agent != "" {
			result.agent.Name = data.Agent
		}
		if result.agent.Model == "" {
			if data.ModelID != "" {
				result.agent.Model = data.ModelID
			} else if data.Model.ModelID != "" {
				result.agent.Model = data.Model.ModelID
			}
		}
		if authoritative {
			newestData = data
		}
		if data.Role == "assistant" && len(data.Error) > 0 && string(data.Error) != "null" {
			if state.assistantError != fingerprint {
				result.eventCount++
				if emitEvents {
					body := assistantError(data.Error)
					groups = append(groups, eventGroup{
						effective: firstPositive(data.Time.Completed, row.timeUpdated, row.timeCreated),
						created:   row.timeCreated,
						rowID:     row.id,
						events: []model.Event{{
							AgentID: result.agent.ID,
							TS:      unixMillis(firstPositive(data.Time.Completed, row.timeUpdated, row.timeCreated)),
							Kind:    model.EvNotice,
							Header:  provider.FirstLine(body, 160),
							Body:    body,
							Lines:   provider.CountLines(body),
							Err:     true,
						}},
					})
					recordEventGroup(stats)
				}
				state.assistantError = fingerprint
			}
		}
		result.state[row.id] = state
	}

	if result.agent.Name != "" {
		suffix := " (@" + result.agent.Name + " subagent)"
		result.agent.Title = strings.TrimSuffix(result.agent.Title, suffix)
	}

	for _, row := range parts {
		result.agent.Updated = laterTime(result.agent.Updated, unixMillis(row.timeUpdated))
		fingerprint := rowFingerprint(row.data)
		state := prior[row.id]
		if state.fingerprint != fingerprint {
			state.malformed = ""
		}
		state.fingerprint = fingerprint

		var header partHeader
		if err := json.Unmarshal([]byte(row.data), &header); err != nil {
			if state.malformed != fingerprint {
				result.eventCount++
				if emitEvents {
					notice := malformedNotice(result.agent.ID, "part", row.id, err, row.timeUpdated, row.timeCreated)
					groups = append(groups, eventGroup{effective: eventMillis(notice.TS), created: row.timeCreated, rowID: row.id, events: []model.Event{notice}})
					recordEventGroup(stats)
				}
				state.malformed = fingerprint
			}
			result.state[row.id] = state
			continue
		}
		if header.Type != "text" && header.Type != "reasoning" && header.Type != "tool" {
			result.state[row.id] = state
			continue
		}

		var data partData
		if err := json.Unmarshal([]byte(row.data), &data); err != nil {
			if state.malformed != fingerprint {
				result.eventCount++
				if emitEvents {
					notice := malformedNotice(result.agent.ID, "part", row.id, err, row.timeUpdated, row.timeCreated)
					groups = append(groups, eventGroup{effective: eventMillis(notice.TS), created: row.timeCreated, rowID: row.id, events: []model.Event{notice}})
					recordEventGroup(stats)
				}
				state.malformed = fingerprint
			}
			result.state[row.id] = state
			continue
		}

		message, hasMessage := messageByID[row.messageID]
		switch data.Type {
		case "text":
			if message.Role == "user" {
				if !data.Synthetic && strings.TrimSpace(data.Text) != "" {
					if result.agent.Prompt == "" {
						result.agent.Prompt = provider.Clip(data.Text, 4000)
					}
					if !state.user {
						result.eventCount++
						if emitEvents {
							groups = append(groups, textGroup(result.agent.ID, row, data.Time.Start, model.EvUser, data.Text))
							recordEventGroup(stats)
						}
						state.user = true
					}
				}
			} else if hasMessage && message.Role == "assistant" && data.Time.End != nil && strings.TrimSpace(data.Text) != "" {
				if !state.textDone {
					result.eventCount++
					if emitEvents {
						groups = append(groups, textGroup(result.agent.ID, row, data.Time.Start, model.EvText, data.Text))
						recordEventGroup(stats)
					}
				}
				state.textDone = true
			}
			if !emitEvents {
				state.user = true
				state.textDone = true
			}

		case "reasoning":
			if hasMessage && message.Role == "assistant" && data.Time.End != nil && strings.TrimSpace(data.Text) != "" {
				if !state.reasoningDone {
					result.eventCount++
					if emitEvents {
						groups = append(groups, textGroup(result.agent.ID, row, data.Time.Start, model.EvReasoning, data.Text))
						recordEventGroup(stats)
					}
				}
				state.reasoningDone = true
			}
			if !emitEvents {
				state.reasoningDone = true
			}

		case "tool":
			useTS := firstPositive(data.State.Time.Start, row.timeCreated)
			terminal := data.State.Status == "completed" || data.State.Status == "error" || data.State.Status == "errored"
			emitUse := !state.toolUse
			emitResult := terminal && !state.toolResult
			var events []model.Event
			groupTime := useTS
			if emitUse {
				result.eventCount++
				if emitEvents {
					body := provider.PrettyJSON(data.State.Input)
					if stats != nil {
						stats.toolBodies++
					}
					events = append(events, model.Event{
						AgentID: result.agent.ID,
						TS:      unixMillis(useTS),
						Kind:    model.EvToolUse,
						Tool:    data.Tool,
						Header:  provider.ToolHeader(data.Tool, data.State.Input),
						Body:    body,
						Lines:   provider.CountLines(body),
					})
				}
				state.toolUse = true
			}
			if emitResult {
				result.eventCount++
				isError := data.State.Status == "error" || data.State.Status == "errored"
				resultTS := firstPositive(derefMillis(data.State.Time.End), row.timeUpdated, row.timeCreated)
				if emitEvents {
					resultBody := data.State.Output
					if isError {
						resultBody = data.State.Error
					}
					head := provider.FirstLine(resultBody, 160)
					if head == "" {
						head = "(no output)"
					}
					events = append(events, model.Event{
						AgentID:  result.agent.ID,
						TS:       unixMillis(resultTS),
						Kind:     model.EvToolResult,
						Tool:     data.Tool,
						Header:   head,
						Body:     resultBody,
						Lines:    provider.CountLines(resultBody),
						Err:      isError,
						Overflow: provider.DetectOverflow(resultBody),
					})
				}
				state.toolResult = true
				if len(events) == 1 {
					groupTime = resultTS
				}
			}
			if !emitEvents {
				state.toolUse = true
				state.toolResult = true
			}
			if len(events) > 0 {
				groups = append(groups, eventGroup{effective: groupTime, created: row.timeCreated, rowID: row.id, events: events})
				recordEventGroup(stats)
			}
		}
		result.state[row.id] = state
	}

	if newest != nil {
		if newestMalformed {
			result.status = model.StatusLive
			result.statusTime = unixMillis(firstPositive(newest.timeUpdated, newest.timeCreated))
		} else {
			result.status, result.statusTime = messageStatus(*newest, newestData)
		}
		result.agent.Status = result.status
	}
	for _, state := range result.state {
		if state.toolUse {
			result.agent.ToolCount++
		}
	}

	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].effective != groups[j].effective {
			return groups[i].effective < groups[j].effective
		}
		if groups[i].created != groups[j].created {
			return groups[i].created < groups[j].created
		}
		return groups[i].rowID < groups[j].rowID
	})
	for _, group := range groups {
		result.events = append(result.events, group.events...)
	}
	return result, nil
}

func sessionDepth(session sessionRow, sessions []sessionRow) (int, error) {
	parents := make(map[string]string, len(sessions))
	for _, row := range sessions {
		parents[row.id] = row.parentID
	}
	depth := 0
	seen := map[string]bool{session.id: true}
	parent := session.parentID
	for parent != "" {
		depth++
		if seen[parent] {
			return 0, fmt.Errorf("OpenCode session parent cycle at %q", parent)
		}
		seen[parent] = true
		next, ok := parents[parent]
		if !ok {
			break
		}
		parent = next
	}
	return depth, nil
}

func recordEventGroup(stats *normalizationStats) {
	if stats != nil {
		stats.eventGroups++
	}
}

func textGroup(agentID string, row partRow, start int64, kind model.EventKind, body string) eventGroup {
	ts := firstPositive(start, row.timeCreated)
	return eventGroup{
		effective: ts,
		created:   row.timeCreated,
		rowID:     row.id,
		events: []model.Event{{
			AgentID: agentID,
			TS:      unixMillis(ts),
			Kind:    kind,
			Header:  provider.FirstLine(body, 160),
			Body:    body,
			Lines:   provider.CountLines(body),
		}},
	}
}

func malformedNotice(agentID, rowType, rowID string, err error, preferred, fallback int64) model.Event {
	body := fmt.Sprintf("malformed OpenCode %s %s: %v", rowType, rowID, err)
	return model.Event{
		AgentID: agentID,
		TS:      unixMillis(firstPositive(preferred, fallback)),
		Kind:    model.EvNotice,
		Header:  provider.FirstLine(body, 160),
		Body:    body,
		Lines:   provider.CountLines(body),
		Err:     true,
	}
}

func assistantError(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	if object, ok := value.(map[string]any); ok {
		if data, ok := object["data"].(map[string]any); ok {
			if message, ok := data["message"].(string); ok && message != "" {
				return message
			}
		}
		for _, key := range []string{"message", "name"} {
			if text, ok := object[key].(string); ok && text != "" {
				return text
			}
		}
	}
	return provider.PrettyJSON(value)
}

func messageStatus(row messageRow, data messageData) (model.Status, time.Time) {
	if data.Role != "assistant" {
		return model.StatusLive, unixMillis(firstPositive(row.timeUpdated, row.timeCreated))
	}
	ts := firstPositive(data.Time.Completed, row.timeUpdated, row.timeCreated)
	if len(data.Error) > 0 && string(data.Error) != "null" || data.Finish == "error" || data.Finish == "content-filter" {
		return model.StatusError, unixMillis(ts)
	}
	if data.Time.Completed > 0 {
		switch data.Finish {
		case "stop", "length", "unknown":
			return model.StatusDone, unixMillis(ts)
		}
	}
	return model.StatusLive, unixMillis(ts)
}

func rowFingerprint(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func unixMillis(ms int64) time.Time {
	return time.UnixMilli(ms)
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func derefMillis(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func eventMillis(value time.Time) int64 {
	return value.UnixMilli()
}
