// Package msgstore keeps a local SQLite copy of message content so it can be
// read back later (the MCP server uses it for chat history and search).
//
// It is opt-in (MESSAGE_STORE_ENABLED) and deliberately independent from the
// GORM "messages" table, which only tracks delivery status.
package msgstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Message is one stored chat message.
type Message struct {
	ID         string
	ChatJID    string
	SenderJID  string
	SenderName string
	FromMe     bool
	Type       string
	Text       string
	QuotedID   string
	Timestamp  time.Time
	Deleted    bool
}

// Chat summarizes one conversation.
type Chat struct {
	JID             string
	Name            string
	MessageCount    int
	LastMessageAt   time.Time
	LastMessageText string
	LastMessageType string
	LastFromMe      bool
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	instance_id TEXT NOT NULL,
	chat_jid    TEXT NOT NULL,
	message_id  TEXT NOT NULL,
	sender_jid  TEXT NOT NULL DEFAULT '',
	sender_name TEXT NOT NULL DEFAULT '',
	from_me     INTEGER NOT NULL DEFAULT 0,
	type        TEXT NOT NULL DEFAULT '',
	text        TEXT NOT NULL DEFAULT '',
	quoted_id   TEXT NOT NULL DEFAULT '',
	timestamp   INTEGER NOT NULL,
	deleted     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (instance_id, chat_jid, message_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages (instance_id, chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages (timestamp);
CREATE TABLE IF NOT EXISTS chats (
	instance_id TEXT NOT NULL,
	chat_jid    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (instance_id, chat_jid)
);
`

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o751); err != nil {
			return nil, fmt.Errorf("create message store directory: %w", err)
		}
	}

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open message store: %w", err)
	}
	// One writer at a time; WAL keeps this cheap and avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate message store: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Save inserts messages. Messages already present are left untouched, so
// replays (history sync, duplicated events) never overwrite edits or deletes.
func (s *Store) Save(instanceID string, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO messages
		(instance_id, chat_jid, message_id, sender_jid, sender_name, from_me, type, text, quoted_id, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance_id, chat_jid, message_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range msgs {
		if m.ID == "" || m.ChatJID == "" {
			continue
		}
		if _, err := stmt.Exec(instanceID, m.ChatJID, m.ID, m.SenderJID, m.SenderName, m.FromMe, m.Type, m.Text, m.QuotedID, m.Timestamp.Unix()); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// EditText replaces the text of an existing message (WhatsApp message edit).
func (s *Store) EditText(instanceID, chatJID, messageID, text string) error {
	_, err := s.db.Exec(`UPDATE messages SET text = ? WHERE instance_id = ? AND chat_jid = ? AND message_id = ?`,
		text, instanceID, chatJID, messageID)
	return err
}

// MarkDeleted flags a message as deleted for everyone and drops its content.
func (s *Store) MarkDeleted(instanceID, chatJID, messageID string) error {
	_, err := s.db.Exec(`UPDATE messages SET deleted = 1, text = '' WHERE instance_id = ? AND chat_jid = ? AND message_id = ?`,
		instanceID, chatJID, messageID)
	return err
}

// SetChatName records a display name for a chat. Empty names are ignored.
func (s *Store) SetChatName(instanceID, chatJID, name string) error {
	if name == "" || chatJID == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO chats (instance_id, chat_jid, name) VALUES (?, ?, ?)
		ON CONFLICT (instance_id, chat_jid) DO UPDATE SET name = excluded.name`,
		instanceID, chatJID, name)
	return err
}

// ChatName returns the stored display name of a chat, or "".
func (s *Store) ChatName(instanceID, chatJID string) string {
	var name string
	_ = s.db.QueryRow(`SELECT name FROM chats WHERE instance_id = ? AND chat_jid = ?`, instanceID, chatJID).Scan(&name)
	return name
}

// ListChats returns chats ordered by most recent activity. query, when set,
// filters by chat name or JID.
func (s *Store) ListChats(instanceID, query string, limit int) ([]Chat, error) {
	args := []any{instanceID}
	where := `m.instance_id = ?`
	if query != "" {
		where += ` AND (c.name LIKE ? ESCAPE '\' OR m.chat_jid LIKE ? ESCAPE '\')`
		pattern := likePattern(query)
		args = append(args, pattern, pattern)
	}
	args = append(args, clampLimit(limit))

	// SQLite fills the bare columns from the row holding MAX(timestamp), which
	// gives us the latest message of each chat without a subquery.
	rows, err := s.db.Query(`SELECT m.chat_jid, COALESCE(c.name, ''), COUNT(*), MAX(m.timestamp), m.text, m.type, m.from_me
		FROM messages m
		LEFT JOIN chats c ON c.instance_id = m.instance_id AND c.chat_jid = m.chat_jid
		WHERE `+where+`
		GROUP BY m.chat_jid
		ORDER BY MAX(m.timestamp) DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		var ts int64
		if err := rows.Scan(&c.JID, &c.Name, &c.MessageCount, &ts, &c.LastMessageText, &c.LastMessageType, &c.LastFromMe); err != nil {
			return nil, err
		}
		c.LastMessageAt = time.Unix(ts, 0)
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// Messages returns up to limit messages of a chat, oldest first. When before
// is non-zero only messages strictly older than it are returned, which lets
// callers page backwards through history.
func (s *Store) Messages(instanceID, chatJID string, before time.Time, limit int) ([]Message, error) {
	args := []any{instanceID, chatJID}
	where := `instance_id = ? AND chat_jid = ?`
	if !before.IsZero() {
		where += ` AND timestamp < ?`
		args = append(args, before.Unix())
	}
	args = append(args, clampLimit(limit))

	msgs, err := s.queryMessages(`SELECT `+messageColumns+` FROM messages WHERE `+where+` ORDER BY timestamp DESC, rowid DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// Search returns messages whose text contains query, newest first. chatJID is
// optional and restricts the search to one chat.
func (s *Store) Search(instanceID, query, chatJID string, limit int) ([]Message, error) {
	args := []any{instanceID, likePattern(query)}
	where := `instance_id = ? AND deleted = 0 AND text LIKE ? ESCAPE '\'`
	if chatJID != "" {
		where += ` AND chat_jid = ?`
		args = append(args, chatJID)
	}
	args = append(args, clampLimit(limit))

	return s.queryMessages(`SELECT `+messageColumns+` FROM messages WHERE `+where+` ORDER BY timestamp DESC, rowid DESC LIMIT ?`, args...)
}

// Get returns one message of a chat, or nil if it is not stored.
func (s *Store) Get(instanceID, chatJID, messageID string) (*Message, error) {
	msgs, err := s.queryMessages(`SELECT `+messageColumns+` FROM messages WHERE instance_id = ? AND chat_jid = ? AND message_id = ?`,
		instanceID, chatJID, messageID)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	return &msgs[0], nil
}

// PurgeInstance removes everything stored for an instance.
func (s *Store) PurgeInstance(instanceID string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE instance_id = ?`, instanceID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM chats WHERE instance_id = ?`, instanceID)
	return err
}

// Prune deletes messages older than cutoff and returns how many were removed.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE timestamp < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const messageColumns = `message_id, chat_jid, sender_jid, sender_name, from_me, type, text, quoted_id, timestamp, deleted`

func (s *Store) queryMessages(query string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var ts int64
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &m.FromMe, &m.Type, &m.Text, &m.QuotedID, &ts, &m.Deleted); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func likePattern(q string) string {
	return "%" + likeEscaper.Replace(q) + "%"
}

// The process-wide store. It is nil unless message persistence is enabled, and
// every package-level helper below is a no-op in that case, so call sites in
// the WhatsApp event/send paths stay one-liners.
var defaultStore atomic.Pointer[Store]

func SetDefault(s *Store) { defaultStore.Store(s) }

func Default() *Store { return defaultStore.Load() }
