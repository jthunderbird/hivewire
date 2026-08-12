package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type sessionRow struct {
	id, parentID, directory, title, version, agent, model string
	timeCreated, timeUpdated                              int64
	tokensInput, tokensOutput, tokensReasoning            int64
	tokensCacheRead, tokensCacheWrite                     int64
}

type messageRow struct {
	id, sessionID, data      string
	timeCreated, timeUpdated int64
}

type partRow struct {
	id, messageID, sessionID, data string
	timeCreated, timeUpdated       int64
}

type dbSnapshot struct {
	present         bool
	sessions        []sessionRow
	messages        map[string][]messageRow
	parts           map[string][]partRow
	missingMessages map[string][]string
	missingParts    map[string][]string
}

type database struct {
	path     string
	lastPoll pollStats
	fileInfo os.FileInfo
	replaced bool
}

type pollStats struct {
	messageRows     int
	partRows        int
	maxQueryArgs    int
	queryCount      int
	watchQueries    int
	maxWatchPayload int
}

const maxWatchPayloadBytes = 64 * 1024

type rowCursor struct {
	updated  int64
	frontier map[string]string
}

type pollRequest struct {
	skip           bool
	skipRows       bool
	updatedSince   int64
	messages       rowCursor
	parts          rowCursor
	messageWatches map[string]string
	partWatches    map[string]string
}

func newDatabase(path string) *database {
	return &database{path: path}
}

func (d *database) snapshot(ctx context.Context) (result dbSnapshot, retErr error) {
	result = dbSnapshot{
		messages: make(map[string][]messageRow),
		parts:    make(map[string][]partRow),
	}
	db, exists, err := openDatabase(ctx, d.path)
	if err != nil || !exists {
		return result, err
	}
	result.present = true
	defer func() {
		if err := db.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	result.sessions, err = querySessions(ctx, tx)
	d.lastPoll.queryCount++
	if err == nil {
		for _, session := range result.sessions {
			result.messages[session.id], err = queryMessages(ctx, tx, session.id)
			if err != nil {
				break
			}
			result.parts[session.id], err = queryParts(ctx, tx, session.id)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return dbSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return dbSnapshot{}, err
	}
	return result, nil
}

func (d *database) pollSnapshot(ctx context.Context, request func(sessionRow) pollRequest) (result dbSnapshot, retErr error) {
	d.lastPoll = pollStats{}
	d.replaced = false
	result = dbSnapshot{
		messages: make(map[string][]messageRow), parts: make(map[string][]partRow),
		missingMessages: make(map[string][]string), missingParts: make(map[string][]string),
	}
	info, err := os.Stat(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if d.fileInfo != nil && !os.SameFile(d.fileInfo, info) {
		d.replaced = true
	}
	d.fileInfo = info
	db, exists, err := openDatabase(ctx, d.path)
	if err != nil || !exists {
		return result, err
	}
	result.present = true
	defer func() {
		if err := db.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result.sessions, err = querySessions(ctx, tx)
	d.lastPoll.queryCount++
	if err != nil {
		return dbSnapshot{}, err
	}
	for _, session := range result.sessions {
		req := request(session)
		if req.skip {
			continue
		}
		args := 1
		if req.updatedSince > 0 || req.messages.updated > 0 || req.parts.updated > 0 {
			args = 2
		}
		if args > d.lastPoll.maxQueryArgs {
			d.lastPoll.maxQueryArgs = args
		}
		if !req.skipRows {
			result.messages[session.id], err = queryMessagesIncremental(ctx, tx, session.id, req.updatedSince, req.messages)
			d.lastPoll.queryCount++
			if err != nil {
				return dbSnapshot{}, err
			}
			result.parts[session.id], err = queryPartsIncremental(ctx, tx, session.id, req.updatedSince, req.parts)
			d.lastPoll.queryCount++
			if err != nil {
				return dbSnapshot{}, err
			}
		}
		if len(req.messageWatches) > 0 {
			rows, missing, err := d.queryMessageWatches(ctx, tx, session.id, req.messageWatches)
			if err != nil {
				return dbSnapshot{}, err
			}
			for _, row := range rows {
				if !hasMessage(result.messages[session.id], row.id) {
					result.messages[session.id] = append(result.messages[session.id], row)
				}
			}
			result.missingMessages[session.id] = append(result.missingMessages[session.id], missing...)
		}
		if len(req.partWatches) > 0 {
			rows, missing, err := d.queryPartWatches(ctx, tx, session.id, req.partWatches)
			if err != nil {
				return dbSnapshot{}, err
			}
			for _, row := range rows {
				if !hasPart(result.parts[session.id], row.id) {
					result.parts[session.id] = append(result.parts[session.id], row)
				}
			}
			result.missingParts[session.id] = append(result.missingParts[session.id], missing...)
		}
		d.lastPoll.messageRows += len(result.messages[session.id])
		d.lastPoll.partRows += len(result.parts[session.id])
	}
	if err := tx.Commit(); err != nil {
		return dbSnapshot{}, err
	}
	return result, nil
}

func (d *database) queryPartWatches(ctx context.Context, tx *sql.Tx, sessionID string, watches map[string]string) ([]partRow, []string, error) {
	var result []partRow
	existing := make(map[string]bool)
	for _, batch := range watchBatches(watches) {
		raw, err := json.Marshal(batch)
		if err != nil {
			return nil, nil, err
		}
		d.recordWatchQuery(len(raw))
		rows, err := tx.QueryContext(ctx, `SELECT p.id,p.message_id,p.session_id,p.time_created,p.time_updated,p.data,w.value
		FROM part p JOIN json_each(?) w ON w.key = p.id
		WHERE p.session_id = ?`, string(raw), sessionID)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var row partRow
			var fingerprint string
			if err := rows.Scan(&row.id, &row.messageID, &row.sessionID, &row.timeCreated, &row.timeUpdated, &row.data, &fingerprint); err != nil {
				rows.Close()
				return nil, nil, err
			}
			existing[row.id] = true
			if rowFingerprint(row.data) != fingerprint {
				result = append(result, row)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, nil, err
		}
	}
	return result, missingWatchIDs(watches, existing), nil
}

func (d *database) queryMessageWatches(ctx context.Context, tx *sql.Tx, sessionID string, watches map[string]string) ([]messageRow, []string, error) {
	var result []messageRow
	existing := make(map[string]bool)
	for _, batch := range watchBatches(watches) {
		raw, err := json.Marshal(batch)
		if err != nil {
			return nil, nil, err
		}
		d.recordWatchQuery(len(raw))
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.session_id,m.time_created,m.time_updated,m.data,w.value
		FROM message m JOIN json_each(?) w ON w.key = m.id
		WHERE m.session_id = ?`, string(raw), sessionID)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var row messageRow
			var fingerprint string
			if err := rows.Scan(&row.id, &row.sessionID, &row.timeCreated, &row.timeUpdated, &row.data, &fingerprint); err != nil {
				rows.Close()
				return nil, nil, err
			}
			existing[row.id] = true
			if rowFingerprint(row.data) != fingerprint {
				result = append(result, row)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, nil, err
		}
	}
	return result, missingWatchIDs(watches, existing), nil
}

func (d *database) recordWatchQuery(payload int) {
	d.lastPoll.queryCount++
	d.lastPoll.watchQueries++
	if payload > d.lastPoll.maxWatchPayload {
		d.lastPoll.maxWatchPayload = payload
	}
	if d.lastPoll.maxQueryArgs < 2 {
		d.lastPoll.maxQueryArgs = 2
	}
}

func watchBatches(watches map[string]string) []map[string]string {
	var batches []map[string]string
	batch := make(map[string]string)
	size := 2
	for id, fingerprint := range watches {
		entrySize := len(id) + len(fingerprint) + 6
		if len(batch) > 0 && size+entrySize > maxWatchPayloadBytes {
			batches = append(batches, batch)
			batch = make(map[string]string)
			size = 2
		}
		batch[id] = fingerprint
		size += entrySize
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

func missingWatchIDs(watches map[string]string, existing map[string]bool) []string {
	var result []string
	for id := range watches {
		if !existing[id] {
			result = append(result, id)
		}
	}
	return result
}

func hasMessage(rows []messageRow, id string) bool {
	for _, row := range rows {
		if row.id == id {
			return true
		}
	}
	return false
}

func hasPart(rows []partRow, id string) bool {
	for _, row := range rows {
		if row.id == id {
			return true
		}
	}
	return false
}

func openDatabase(ctx context.Context, path string) (*sql.DB, bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, false, err
	}
	u := sqliteFileURI(abs)

	db, err := sql.Open("sqlite", u)
	if err != nil {
		return nil, false, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, false, err
	}
	return db, true, nil
}

func sqliteFileURI(path string) string {
	u := sqliteFileURL(path)
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(50)")
	u.RawQuery = q.Encode()
	return u.String()
}

func sqliteFileURL(path string) url.URL {
	path = strings.ReplaceAll(path, `\`, "/")
	if len(path) >= 2 && path[1] == ':' {
		path = "/" + path
	}
	return url.URL{Scheme: "file", Path: path}
}

func querySessions(ctx context.Context, tx *sql.Tx) ([]sessionRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		id,parent_id,directory,title,version,agent,model,
		tokens_input,tokens_output,tokens_reasoning,tokens_cache_read,tokens_cache_write,
		time_created,time_updated
		FROM session
		WHERE parent_id IS NOT NULL AND parent_id <> ''
		ORDER BY time_created,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []sessionRow
	for rows.Next() {
		var row sessionRow
		var agent, model sql.NullString
		if err := rows.Scan(
			&row.id, &row.parentID, &row.directory, &row.title, &row.version, &agent, &model,
			&row.tokensInput, &row.tokensOutput, &row.tokensReasoning, &row.tokensCacheRead, &row.tokensCacheWrite,
			&row.timeCreated, &row.timeUpdated,
		); err != nil {
			return nil, err
		}
		row.agent, row.model = agent.String, model.String
		result = append(result, row)
	}
	return result, rows.Err()
}

func queryMessages(ctx context.Context, tx *sql.Tx, sessionID string) ([]messageRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,session_id,time_created,time_updated,data
		FROM message WHERE session_id = ? ORDER BY time_created,id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []messageRow
	for rows.Next() {
		var row messageRow
		if err := rows.Scan(&row.id, &row.sessionID, &row.timeCreated, &row.timeUpdated, &row.data); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func queryParts(ctx context.Context, tx *sql.Tx, sessionID string) ([]partRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,message_id,session_id,time_created,time_updated,data
		FROM part WHERE session_id = ? ORDER BY time_created,id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []partRow
	for rows.Next() {
		var row partRow
		if err := rows.Scan(&row.id, &row.messageID, &row.sessionID, &row.timeCreated, &row.timeUpdated, &row.data); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func queryMessagesIncremental(ctx context.Context, tx *sql.Tx, sessionID string, updatedSince int64, cursor rowCursor) ([]messageRow, error) {
	query := `SELECT id,session_id,time_created,time_updated,data FROM message WHERE session_id = ?`
	args := []any{sessionID}
	if updatedSince > 0 {
		query += ` AND time_updated >= ?`
		args = append(args, updatedSince)
	} else if cursor.updated > 0 {
		query += ` AND time_updated >= ?`
		args = append(args, cursor.updated)
	}
	if len(args) > 2 {
		panic("incremental message query has unbounded arguments")
	}
	query += ` ORDER BY time_created,id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []messageRow
	for rows.Next() {
		var row messageRow
		if err := rows.Scan(&row.id, &row.sessionID, &row.timeCreated, &row.timeUpdated, &row.data); err != nil {
			return nil, err
		}
		if row.timeUpdated == cursor.updated && cursor.frontier[row.id] == rowFingerprint(row.data) {
			continue
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func queryPartsIncremental(ctx context.Context, tx *sql.Tx, sessionID string, updatedSince int64, cursor rowCursor) ([]partRow, error) {
	query := `SELECT id,message_id,session_id,time_created,time_updated,data FROM part WHERE session_id = ?`
	args := []any{sessionID}
	if updatedSince > 0 {
		query += ` AND time_updated >= ?`
		args = append(args, updatedSince)
	} else if cursor.updated > 0 {
		query += ` AND time_updated >= ?`
		args = append(args, cursor.updated)
	}
	if len(args) > 2 {
		panic("incremental part query has unbounded arguments")
	}
	query += ` ORDER BY time_created,id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []partRow
	for rows.Next() {
		var row partRow
		if err := rows.Scan(&row.id, &row.messageID, &row.sessionID, &row.timeCreated, &row.timeUpdated, &row.data); err != nil {
			return nil, err
		}
		if row.timeUpdated == cursor.updated && cursor.frontier[row.id] == rowFingerprint(row.data) {
			continue
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
