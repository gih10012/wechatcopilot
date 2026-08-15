package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gih10012/wechatcopilot/internal/driver"
	_ "modernc.org/sqlite"
)

type Store struct {
	db        *sql.DB
	accountID string
}

func Open(path, accountID string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, accountID: accountID}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout=5000`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY,
			external_id TEXT NOT NULL,
			title TEXT NOT NULL,
			kind TEXT NOT NULL,
			unread_count INTEGER NOT NULL DEFAULT 0,
			last_message_at TEXT,
			complete INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS conversations_external_id ON conversations(external_id)`,
		`CREATE TABLE IF NOT EXISTS messages (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			id TEXT NOT NULL UNIQUE,
			external_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL,
			sender_id TEXT,
			sender_name TEXT,
			sent_at TEXT NOT NULL,
			kind TEXT NOT NULL,
			text TEXT,
			attachments_json TEXT NOT NULL DEFAULT '[]',
			reply_to TEXT,
			surface_ref TEXT,
			source TEXT NOT NULL,
			complete INTEGER NOT NULL,
			confidence REAL NOT NULL,
			raw_json BLOB,
			inserted_at TEXT NOT NULL,
			FOREIGN KEY(conversation_id) REFERENCES conversations(id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS messages_external_conversation ON messages(conversation_id, external_id)`,
		`CREATE INDEX IF NOT EXISTS messages_conversation_sequence ON messages(conversation_id, sequence)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(message_id UNINDEXED, text, sender_name, tokenize='unicode61')`,
		`CREATE TABLE IF NOT EXISTS send_results (
			idempotency_key TEXT PRIMARY KEY,
			transaction_id TEXT NOT NULL,
			request_hash TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'terminal',
			result_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS send_results_transaction ON send_results(transaction_id)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize message index: %w", err)
		}
	}
	if err := s.ensureSendStateColumn(ctx); err != nil {
		return fmt.Errorf("initialize send journal: %w", err)
	}
	return nil
}

func (s *Store) ensureSendStateColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(send_results)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "state" {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE send_results ADD COLUMN state TEXT NOT NULL DEFAULT 'terminal'`)
	return err
}

func (s *Store) UpsertConversations(ctx context.Context, conversations []driver.Conversation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range conversations {
		if item.ID == "" || item.ExternalID == "" {
			return errors.New("conversation id and external id are required")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO conversations
			(id, external_id, title, kind, unread_count, last_message_at, complete, source, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				external_id=excluded.external_id,
				title=excluded.title,
				kind=excluded.kind,
				unread_count=excluded.unread_count,
				last_message_at=excluded.last_message_at,
				complete=excluded.complete,
				source=excluded.source,
				updated_at=excluded.updated_at`,
			item.ID, item.ExternalID, item.Title, item.Kind, item.UnreadCount, nullableTime(item.LastMessageAt), item.Complete, item.Source, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddMessages(ctx context.Context, messages []driver.Message) ([]driver.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	inserted := make([]driver.Message, 0, len(messages))
	for _, message := range messages {
		if message.ID == "" || message.ExternalID == "" || message.ConversationID == "" {
			return nil, errors.New("message id, external id, and conversation id are required")
		}
		attachments, err := json.Marshal(message.Attachments)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO messages
			(id, external_id, conversation_id, sender_id, sender_name, sent_at, kind, text,
			 attachments_json, reply_to, surface_ref, source, complete, confidence, raw_json, inserted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			message.ID, message.ExternalID, message.ConversationID, message.SenderID, message.SenderName,
			message.SentAt.UTC().Format(time.RFC3339Nano), message.Kind, message.Text, string(attachments),
			message.ReplyTo, message.SurfaceRef, message.Source, message.Complete, message.Confidence,
			[]byte(message.Raw), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return nil, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if rows == 0 {
			continue
		}
		if err := tx.QueryRowContext(ctx, `SELECT sequence FROM messages WHERE id=?`, message.ID).Scan(&message.Sequence); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(message_id, text, sender_name) VALUES (?, ?, ?)`, message.ID, message.Text, message.SenderName); err != nil {
			return nil, err
		}
		inserted = append(inserted, message)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return inserted, nil
}

func (s *Store) ListConversations(ctx context.Context, search string, unread bool, limit int) ([]driver.Conversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id, external_id, title, kind, unread_count, COALESCE(last_message_at, ''), complete, source
		FROM conversations WHERE 1=1`
	var args []any
	if search != "" {
		query += ` AND title LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(search)+"%")
	}
	if unread {
		query += ` AND unread_count > 0`
	}
	query += ` ORDER BY last_message_at DESC, title ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []driver.Conversation
	for rows.Next() {
		var item driver.Conversation
		var timestamp string
		if err := rows.Scan(&item.ID, &item.ExternalID, &item.Title, &item.Kind, &item.UnreadCount, &timestamp, &item.Complete, &item.Source); err != nil {
			return nil, err
		}
		if timestamp != "" {
			item.LastMessageAt, _ = time.Parse(time.RFC3339Nano, timestamp)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListMessages(ctx context.Context, query driver.MessageQuery) ([]driver.Message, error) {
	limit := query.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	columns := `sequence, id, external_id, conversation_id, COALESCE(sender_id,''), COALESCE(sender_name,''),
		sent_at, kind, COALESCE(text,''), attachments_json, COALESCE(reply_to,''), COALESCE(surface_ref,''),
		source, complete, confidence, COALESCE(raw_json, '')`
	statement := `SELECT ` + columns + ` FROM messages WHERE sequence > ?`
	args := []any{query.AfterSequence}
	if query.ConversationID != "" {
		statement += ` AND conversation_id = ?`
		args = append(args, query.ConversationID)
	}
	if !query.Before.IsZero() {
		statement += ` AND sent_at < ?`
		args = append(args, query.Before.UTC().Format(time.RFC3339Nano))
	}
	if query.Latest {
		statement = `SELECT * FROM (` + statement + ` ORDER BY sequence DESC LIMIT ?) ORDER BY sequence ASC`
	} else {
		statement += ` ORDER BY sequence ASC LIMIT ?`
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []driver.Message
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) SearchMessages(ctx context.Context, text string, limit int) ([]driver.Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("search text is required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.sequence, m.id, m.external_id, m.conversation_id,
		COALESCE(m.sender_id,''), COALESCE(m.sender_name,''), m.sent_at, m.kind, COALESCE(m.text,''),
		m.attachments_json, COALESCE(m.reply_to,''), COALESCE(m.surface_ref,''), m.source, m.complete,
		m.confidence, COALESCE(m.raw_json, '')
		FROM messages_fts f JOIN messages m ON m.id=f.message_id
		WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?`, text, limit)
	if err == nil {
		messages, scanErr := collectMessages(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if len(messages) > 0 {
			return messages, nil
		}
	}

	// unicode61 does not segment every CJK phrase or support arbitrary literal
	// punctuation. A parameterized LIKE fallback preserves literal substring
	// search without exposing SQL or FTS syntax.
	pattern := "%" + escapeLike(text) + "%"
	rows, err = s.db.QueryContext(ctx, `SELECT sequence, id, external_id, conversation_id,
		COALESCE(sender_id,''), COALESCE(sender_name,''), sent_at, kind, COALESCE(text,''),
		attachments_json, COALESCE(reply_to,''), COALESCE(surface_ref,''), source, complete,
		confidence, COALESCE(raw_json, '') FROM messages
		WHERE text LIKE ? ESCAPE '\' OR sender_name LIKE ? ESCAPE '\'
		ORDER BY sequence DESC LIMIT ?`, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	return collectMessages(rows)
}

func collectMessages(rows *sql.Rows) ([]driver.Message, error) {
	defer rows.Close()
	var messages []driver.Message
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) GetSendResult(ctx context.Context, key, hash string) (*driver.SendResult, error) {
	var storedHash, resultJSON string
	err := s.db.QueryRowContext(ctx, `SELECT request_hash, result_json FROM send_results WHERE idempotency_key=?`, key).Scan(&storedHash, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedHash != hash {
		return nil, errors.New("idempotency key was already used for another request")
	}
	var result driver.SendResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *Store) GetSendResultForTransaction(ctx context.Context, transactionID string) (string, *driver.SendResult, error) {
	var key, resultJSON string
	err := s.db.QueryRowContext(ctx, `SELECT idempotency_key, result_json FROM send_results
		WHERE transaction_id=?`, transactionID).Scan(&key, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var result driver.SendResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return "", nil, err
	}
	return key, &result, nil
}

func (s *Store) SaveSendResult(ctx context.Context, key, transactionID, hash string, result driver.SendResult) error {
	contents, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO send_results
		(idempotency_key, transaction_id, request_hash, state, result_json, created_at) VALUES (?, ?, ?, 'terminal', ?, ?)`,
		key, transactionID, hash, string(contents), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ReserveSend(ctx context.Context, key, transactionID, hash string, result driver.SendResult) error {
	contents, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO send_results
		(idempotency_key, transaction_id, request_hash, state, result_json, created_at) VALUES (?, ?, ?, 'in_flight', ?, ?)`,
		key, transactionID, hash, string(contents), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) FinalizeSend(ctx context.Context, key, transactionID, hash string, result driver.SendResult) error {
	contents, err := json.Marshal(result)
	if err != nil {
		return err
	}
	update, err := s.db.ExecContext(ctx, `UPDATE send_results SET state='terminal', result_json=?
		WHERE idempotency_key=? AND transaction_id=? AND request_hash=? AND state='in_flight'`,
		string(contents), key, transactionID, hash)
	if err != nil {
		return err
	}
	rows, err := update.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("send reservation is missing or no longer in flight")
	}
	return nil
}

func (s *Store) DeleteSendReservation(ctx context.Context, key, transactionID, hash string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM send_results
		WHERE idempotency_key=? AND transaction_id=? AND request_hash=? AND state='in_flight'`,
		key, transactionID, hash)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("send reservation is missing or no longer in flight")
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanMessage(row rowScanner) (driver.Message, error) {
	var message driver.Message
	var timestamp, attachments, raw string
	if err := row.Scan(&message.Sequence, &message.ID, &message.ExternalID, &message.ConversationID,
		&message.SenderID, &message.SenderName, &timestamp, &message.Kind, &message.Text, &attachments,
		&message.ReplyTo, &message.SurfaceRef, &message.Source, &message.Complete, &message.Confidence, &raw); err != nil {
		return driver.Message{}, err
	}
	message.SentAt, _ = time.Parse(time.RFC3339Nano, timestamp)
	if err := json.Unmarshal([]byte(attachments), &message.Attachments); err != nil {
		return driver.Message{}, err
	}
	if json.Valid([]byte(raw)) {
		message.Raw = json.RawMessage(raw)
	}
	return message, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}
