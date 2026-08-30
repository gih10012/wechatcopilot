package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gih10012/wechatcopilot/internal/driver"
	_ "modernc.org/sqlite"
)

type Store struct {
	db        *sql.Conn
	pool      *sql.DB
	accountID string
	pinned    *pinnedIndexPath
}

const indexSchemaVersion = 1

var ErrLegacyIndexUnbound = errors.New("non-empty legacy message index has no account ownership metadata")

func Open(path, accountID string) (*Store, error) {
	return open(path, accountID, false, nil)
}

// AdoptLegacy explicitly binds a recognized pre-metadata index at the exact
// registered account path. Ordinary Open never claims a non-empty legacy DB.
func AdoptLegacy(path, accountID string) (*Store, error) {
	return open(path, accountID, true, nil)
}

type openTestHook func() error

// Opening is serialized only while process descriptors are sampled. A Store
// keeps one dedicated sql.Conn for its entire lifetime, so database/sql can
// never silently reconnect through a pathname that changed after validation.
var indexOpenMu sync.Mutex
var indexAnchorDescriptors = make(map[int]fileIdentity)

func open(path, accountID string, adoptLegacy bool, beforeSQLiteOpen openTestHook) (*Store, error) {
	if strings.TrimSpace(accountID) != accountID || accountID == "" || len(accountID) > 128 || strings.ContainsAny(accountID, "/\\\x00") {
		return nil, errors.New("message index account ID is invalid")
	}
	path = filepath.Clean(path)
	// Ordinary opens may initialize a new account index. Legacy adoption is an
	// offline migration of an existing database and must never create a missing
	// file after a preflight check or concurrent pathname change.
	pinned, err := pinIndexPath(path, accountID, !adoptLegacy)
	if err != nil {
		return nil, err
	}
	if beforeSQLiteOpen != nil {
		if err := beforeSQLiteOpen(); err != nil {
			_ = pinned.close()
			return nil, err
		}
	}
	var pool *sql.DB
	var conn *sql.Conn
	err = func() error {
		indexOpenMu.Lock()
		defer indexOpenMu.Unlock()
		indexAnchorDescriptors[int(pinned.file.Fd())] = pinned.fileID
		var snapshotErr error
		pool, snapshotErr = sql.Open("sqlite", pinned.dsn)
		if snapshotErr != nil {
			return snapshotErr
		}
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		conn, snapshotErr = pool.Conn(context.Background())
		if snapshotErr != nil {
			return fmt.Errorf("acquire pinned message index connection: %w", snapshotErr)
		}
		if _, snapshotErr = conn.ExecContext(context.Background(), `PRAGMA busy_timeout=5000`); snapshotErr != nil {
			return fmt.Errorf("configure pinned message index lock timeout: %w", snapshotErr)
		}
		if snapshotErr = verifySQLiteHasPinnedInode(pinned.fileID, indexAnchorDescriptors); snapshotErr != nil {
			return snapshotErr
		}
		return pinned.verify(path)
	}()
	if err != nil {
		_ = closeIndexResources(conn, pool, pinned)
		return nil, err
	}
	store := &Store{db: conn, pool: pool, accountID: accountID, pinned: pinned}
	if err := store.initialize(context.Background(), adoptLegacy); err != nil {
		_ = closeIndexResources(conn, pool, pinned)
		return nil, err
	}
	if err := pinned.verify(path); err != nil {
		_ = closeIndexResources(conn, pool, pinned)
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return closeIndexResources(s.db, s.pool, s.pinned)
}

func closeIndexResources(conn *sql.Conn, pool *sql.DB, pinned *pinnedIndexPath) error {
	indexOpenMu.Lock()
	defer indexOpenMu.Unlock()
	var connErr, poolErr error
	if conn != nil {
		connErr = conn.Close()
	}
	if pool != nil {
		poolErr = pool.Close()
	}
	if pinned != nil && pinned.file != nil {
		delete(indexAnchorDescriptors, int(pinned.file.Fd()))
	}
	pinErr := pinned.close()
	return errors.Join(connErr, poolErr, pinErr)
}

func (s *Store) initialize(ctx context.Context, adoptLegacy bool) error {
	// Install the lock wait policy before the first schema or ownership read so
	// a concurrent close/checkpoint cannot turn a valid reopen into SQLITE_BUSY.
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return fmt.Errorf("configure message index lock timeout: %w", err)
	}
	metadataExists, err := s.metadataTableExists(ctx)
	if err != nil {
		return fmt.Errorf("inspect message index ownership metadata: %w", err)
	}
	if metadataExists {
		if adoptLegacy {
			return errors.New("message index already contains ownership metadata; legacy adoption is not applicable")
		}
		if err := s.validateMetadata(ctx); err != nil {
			return err
		}
	} else {
		hasSchema, err := s.hasApplicationSchema(ctx)
		if err != nil {
			return fmt.Errorf("inspect legacy message index schema: %w", err)
		}
		if !hasSchema {
			if adoptLegacy {
				return errors.New("message index is empty; explicit legacy adoption is not applicable")
			}
		} else {
			if err := s.validateLegacySchema(ctx); err != nil {
				if !adoptLegacy {
					return errors.Join(ErrLegacyIndexUnbound, err)
				}
				return err
			}
			hasRows, err := s.legacyContainsApplicationData(ctx)
			if err != nil {
				return fmt.Errorf("inspect legacy message index contents: %w", err)
			}
			if hasRows && !adoptLegacy {
				return ErrLegacyIndexUnbound
			}
			if !hasRows && adoptLegacy {
				return errors.New("message index contains no account data; explicit legacy adoption is not applicable")
			}
		}
	}
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure message index: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
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
			gap_before INTEGER NOT NULL DEFAULT 0,
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
		`CREATE TABLE IF NOT EXISTS wechatcopilot_metadata (
			singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
			schema_version INTEGER NOT NULL,
			account_id TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize message index: %w", err)
		}
	}
	if !metadataExists {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO wechatcopilot_metadata(singleton, schema_version, account_id) VALUES (1, ?, ?)`,
			indexSchemaVersion, s.accountID,
		); err != nil {
			return fmt.Errorf("bind message index to account: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.ensureSendStateColumn(ctx); err != nil {
		return fmt.Errorf("initialize send journal: %w", err)
	}
	if err := s.ensureMessageGapBeforeColumn(ctx); err != nil {
		return fmt.Errorf("initialize message gap metadata: %w", err)
	}
	if err := s.validateInitializedSchema(ctx); err != nil {
		return fmt.Errorf("validate message index schema: %w", err)
	}
	if err := s.validateMetadata(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) hasApplicationSchema(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`,
	).Scan(&count)
	return count != 0, err
}

func (s *Store) legacyContainsApplicationData(ctx context.Context) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM conversations LIMIT 1) OR
		EXISTS(SELECT 1 FROM messages LIMIT 1) OR
		EXISTS(SELECT 1 FROM messages_fts LIMIT 1) OR
		EXISTS(SELECT 1 FROM send_results LIMIT 1)`,
	).Scan(&present)
	return present != 0, err
}

func (s *Store) validateLegacySchema(ctx context.Context) error {
	if err := s.validateLegacyObjectSet(ctx); err != nil {
		return fmt.Errorf("legacy message index is not a recognized wechatcopilot schema: %w", err)
	}
	if err := s.validateTableColumns(ctx, "conversations", []string{
		"id", "external_id", "title", "kind", "unread_count", "last_message_at", "complete", "source", "updated_at",
	}); err != nil {
		return fmt.Errorf("legacy message index is not a recognized wechatcopilot schema: %w", err)
	}
	if err := s.validateTableColumnsOneOf(ctx, "messages", [][]string{
		{
			"sequence", "id", "external_id", "conversation_id", "sender_id", "sender_name", "sent_at", "kind", "text",
			"attachments_json", "reply_to", "surface_ref", "source", "complete", "confidence", "raw_json", "inserted_at",
		},
		{
			"sequence", "id", "external_id", "conversation_id", "sender_id", "sender_name", "sent_at", "kind", "text",
			"attachments_json", "reply_to", "surface_ref", "source", "complete", "confidence", "raw_json", "inserted_at", "gap_before",
		},
	}); err != nil {
		return fmt.Errorf("legacy message index is not a recognized wechatcopilot schema: %w", err)
	}
	if err := s.validateTableColumnsOneOf(ctx, "send_results", [][]string{
		{"idempotency_key", "transaction_id", "request_hash", "state", "result_json", "created_at"},
		{"idempotency_key", "transaction_id", "request_hash", "result_json", "created_at"},
	}); err != nil {
		return fmt.Errorf("legacy message index is not a recognized wechatcopilot schema: %w", err)
	}
	var ftsSQL string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='table' AND name='messages_fts'`,
	).Scan(&ftsSQL); err != nil {
		return fmt.Errorf("legacy message index is not a recognized wechatcopilot schema: messages_fts: %w", err)
	}
	ftsSQL = strings.ToLower(strings.Join(strings.Fields(ftsSQL), " "))
	if !strings.Contains(ftsSQL, "create virtual table") || !strings.Contains(ftsSQL, "using fts5") {
		return errors.New("legacy message index is not a recognized wechatcopilot schema: messages_fts is not an FTS5 virtual table")
	}
	var quickCheck string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&quickCheck); err != nil {
		return fmt.Errorf("check legacy message index integrity: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("legacy message index failed integrity check: %s", quickCheck)
	}
	return nil
}

func (s *Store) validateLegacyObjectSet(ctx context.Context) error {
	expected := map[string]string{
		"conversations":                  "table",
		"conversations_external_id":      "index",
		"messages":                       "table",
		"messages_external_conversation": "index",
		"messages_conversation_sequence": "index",
		"messages_fts":                   "table",
		"messages_fts_data":              "table",
		"messages_fts_idx":               "table",
		"messages_fts_content":           "table",
		"messages_fts_docsize":           "table",
		"messages_fts_config":            "table",
		"send_results":                   "table",
		"send_results_transaction":       "index",
	}
	rows, err := s.db.QueryContext(ctx, `SELECT type, name FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[string]bool, len(expected))
	for rows.Next() {
		var objectType, name string
		if err := rows.Scan(&objectType, &name); err != nil {
			return err
		}
		if expectedType, ok := expected[name]; !ok || objectType != expectedType || seen[name] {
			return fmt.Errorf("unexpected schema object %s %s", objectType, name)
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("required legacy schema objects are missing")
	}
	return nil
}

func (s *Store) validateInitializedSchema(ctx context.Context) error {
	if err := s.validateTableColumns(ctx, "conversations", []string{
		"id", "external_id", "title", "kind", "unread_count", "last_message_at", "complete", "source", "updated_at",
	}); err != nil {
		return err
	}
	if err := s.validateTableColumns(ctx, "messages", []string{
		"sequence", "id", "external_id", "conversation_id", "sender_id", "sender_name", "sent_at", "kind", "text",
		"attachments_json", "reply_to", "surface_ref", "source", "complete", "confidence", "raw_json", "inserted_at", "gap_before",
	}); err != nil {
		return err
	}
	return s.validateTableColumns(ctx, "send_results", []string{
		"idempotency_key", "transaction_id", "request_hash", "state", "result_json", "created_at",
	})
}

func (s *Store) validateTableColumns(ctx context.Context, table string, expected []string) error {
	actual, err := s.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("table %s has unexpected columns", table)
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return fmt.Errorf("table %s has unexpected columns", table)
		}
	}
	return nil
}

func (s *Store) validateTableColumnsOneOf(ctx context.Context, table string, choices [][]string) error {
	actual, err := s.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	for _, expected := range choices {
		if len(actual) != len(expected) {
			continue
		}
		matches := true
		for i := range expected {
			if actual[i] != expected[i] {
				matches = false
				break
			}
		}
		if matches {
			return nil
		}
	}
	return fmt.Errorf("table %s has unexpected columns", table)
}

func (s *Store) tableColumns(ctx context.Context, table string) ([]string, error) {
	// Table names are constants owned by this package; quote defensively so
	// this helper cannot accidentally grow into a dynamic SQL primitive.
	if table != "conversations" && table != "messages" && table != "send_results" {
		return nil, errors.New("unsupported message index table inspection")
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, fmt.Errorf("inspect table %s: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %s is missing", table)
	}
	return columns, nil
}

func (s *Store) metadataTableExists(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='wechatcopilot_metadata'`,
	).Scan(&count)
	return count == 1, err
}

func (s *Store) validateMetadata(ctx context.Context) error {
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wechatcopilot_metadata`).Scan(&rows); err != nil {
		return fmt.Errorf("read message index ownership metadata: %w", err)
	}
	if rows != 1 {
		return errors.New("message index ownership metadata is missing or ambiguous")
	}
	var schemaVersion int
	var accountID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT schema_version, account_id FROM wechatcopilot_metadata WHERE singleton=1`,
	).Scan(&schemaVersion, &accountID); err != nil {
		return fmt.Errorf("read message index ownership metadata: %w", err)
	}
	if schemaVersion != indexSchemaVersion {
		return fmt.Errorf("unsupported message index metadata schema %d", schemaVersion)
	}
	if accountID != s.accountID {
		return errors.New("message index belongs to another account")
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

func (s *Store) ensureMessageGapBeforeColumn(ctx context.Context) error {
	columns, err := s.tableColumns(ctx, "messages")
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column == "gap_before" {
			return nil
		}
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN gap_before INTEGER NOT NULL DEFAULT 0`)
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
			 attachments_json, reply_to, surface_ref, source, complete, confidence, raw_json, inserted_at, gap_before)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			message.ID, message.ExternalID, message.ConversationID, message.SenderID, message.SenderName,
			message.SentAt.UTC().Format(time.RFC3339Nano), message.Kind, message.Text, string(attachments),
			message.ReplyTo, message.SurfaceRef, message.Source, message.Complete, message.Confidence,
			[]byte(message.Raw), time.Now().UTC().Format(time.RFC3339Nano), message.GapBefore)
		if err != nil {
			return nil, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if rows == 0 {
			if message.GapBefore {
				updated, err := tx.ExecContext(ctx, `UPDATE messages SET gap_before=1
					WHERE id=? AND external_id=? AND conversation_id=?`,
					message.ID, message.ExternalID, message.ConversationID)
				if err != nil {
					return nil, err
				}
				updatedRows, err := updated.RowsAffected()
				if err != nil {
					return nil, err
				}
				if updatedRows != 1 {
					return nil, errors.New("duplicate message identity does not match the indexed record")
				}
			}
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
		source, complete, confidence, COALESCE(raw_json, ''), gap_before`
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
		m.confidence, COALESCE(m.raw_json, ''), m.gap_before
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
		confidence, COALESCE(raw_json, ''), gap_before FROM messages
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
		&message.ReplyTo, &message.SurfaceRef, &message.Source, &message.Complete, &message.Confidence, &raw,
		&message.GapBefore); err != nil {
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
