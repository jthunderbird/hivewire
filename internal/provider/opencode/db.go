package opencode

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

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
	sessions []sessionRow
	messages map[string][]messageRow
	parts    map[string][]partRow
}

type database struct {
	path string
	db   *sql.DB
	info os.FileInfo
}

func newDatabase(path string) *database {
	return &database{path: path}
}

func (d *database) snapshot(ctx context.Context) (dbSnapshot, error) {
	result := dbSnapshot{
		messages: make(map[string][]messageRow),
		parts:    make(map[string][]partRow),
	}
	exists, err := d.open(ctx)
	if err != nil || !exists {
		return result, err
	}

	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		d.clear()
		return result, err
	}
	defer tx.Rollback()

	result.sessions, err = querySessions(ctx, tx)
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
		d.clear()
		return dbSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		d.clear()
		return dbSnapshot{}, err
	}
	return result, nil
}

func (d *database) open(ctx context.Context) (bool, error) {
	info, err := os.Stat(d.path)
	if errors.Is(err, os.ErrNotExist) {
		d.clear()
		return false, nil
	}
	if err != nil {
		d.clear()
		return false, err
	}
	if d.db != nil {
		if d.info != nil && os.SameFile(d.info, info) {
			return true, nil
		}
		d.clear()
	}

	abs, err := filepath.Abs(d.path)
	if err != nil {
		return false, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(50)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return false, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return false, err
	}
	d.db = db
	d.info = info
	return true, nil
}

func (d *database) clear() {
	if d.db != nil {
		d.db.Close()
	}
	d.db = nil
	d.info = nil
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
