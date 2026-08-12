package opencode

import (
	"context"
	"database/sql"
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
	present  bool
	sessions []sessionRow
	messages map[string][]messageRow
	parts    map[string][]partRow
}

type database struct {
	path     string
	lastPoll pollStats
	fileInfo os.FileInfo
	replaced bool
}

// pollStats records what one poll actually read, so tests can prove the
// incremental path stays quiet when nothing changed.
type pollStats struct {
	queryCount  int
	messageRows int
	partRows    int
}

// rowCursor is a high-water mark over time_updated plus the fingerprints of the
// rows sitting exactly on it, so a mutation that lands in the same millisecond
// is still seen while an unchanged row is not re-emitted.
type rowCursor struct {
	updated  int64
	frontier map[string]string
}

type pollRequest struct {
	skip     bool
	messages rowCursor
	parts    rowCursor
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
		messages: make(map[string][]messageRow),
		parts:    make(map[string][]partRow),
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
		result.messages[session.id], err = queryMessagesIncremental(ctx, tx, session.id, req.messages)
		d.lastPoll.queryCount++
		if err != nil {
			return dbSnapshot{}, err
		}
		result.parts[session.id], err = queryPartsIncremental(ctx, tx, session.id, req.parts)
		d.lastPoll.queryCount++
		if err != nil {
			return dbSnapshot{}, err
		}
		d.lastPoll.messageRows += len(result.messages[session.id])
		d.lastPoll.partRows += len(result.parts[session.id])
	}
	if err := tx.Commit(); err != nil {
		return dbSnapshot{}, err
	}
	return result, nil
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

// queryMessagesIncremental reads only rows at or after the cursor's high-water
// mark, dropping the ones on the mark whose content is unchanged.
func queryMessagesIncremental(ctx context.Context, tx *sql.Tx, sessionID string, cursor rowCursor) ([]messageRow, error) {
	query := `SELECT id,session_id,time_created,time_updated,data FROM message WHERE session_id = ?`
	args := []any{sessionID}
	if cursor.updated > 0 {
		query += ` AND time_updated >= ?`
		args = append(args, cursor.updated)
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

// queryPartsIncremental is queryMessagesIncremental for message parts.
func queryPartsIncremental(ctx context.Context, tx *sql.Tx, sessionID string, cursor rowCursor) ([]partRow, error) {
	query := `SELECT id,message_id,session_id,time_created,time_updated,data FROM part WHERE session_id = ?`
	args := []any{sessionID}
	if cursor.updated > 0 {
		query += ` AND time_updated >= ?`
		args = append(args, cursor.updated)
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
