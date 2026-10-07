package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MessageStore is the local record of chats and messages, plus the list of
// files already saved to folders.
type MessageStore struct {
	db *sql.DB
}

func NewMessageStore() (*MessageStore, error) {
	if err := os.MkdirAll("store", 0700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:store/messages.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_time_format=sqlite")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
		CREATE TABLE IF NOT EXISTS saved_files (
			message_id TEXT,
			chat_jid TEXT,
			saved_path TEXT,
			saved_at TEXT,
			PRIMARY KEY (message_id, chat_jid, saved_path)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}
	// Added after the first release; fails harmlessly when the column exists
	db.Exec("ALTER TABLE messages ADD COLUMN sender_name TEXT")
	return &MessageStore{db: db}, nil
}

func (store *MessageStore) Close() error {
	return store.db.Close()
}

func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

func (store *MessageStore) StoreMessage(id, chatJID, sender, senderName, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	if content == "" && mediaType == "" {
		return nil
	}
	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, sender_name, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, senderName, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// ---- Queries used by the Claude tools ----

type Chat struct {
	JID         string `json:"chat_jid"`
	Name        string `json:"name"`
	IsGroup     bool   `json:"is_group"`
	LastMessage string `json:"last_message_time"`
}

type Msg struct {
	ID        string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	ChatName  string `json:"chat_name"`
	Time      string `json:"time"`
	Sender    string `json:"sender"`
	Number    string `json:"sender_number"`
	FromMe    bool   `json:"from_me"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
	SizeKB    int64  `json:"size_kb,omitempty"`
	SavedTo   string `json:"already_saved_to,omitempty"`
}

func showTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Local().Format("2006-01-02 15:04")
}

// parseWhen accepts "2026-10-07", "2026-10-07 14:30" or "2026-10-07T14:30:00".
func parseWhen(s string) (time.Time, error) {
	s = strings.TrimSpace(strings.Replace(s, "T", " ", 1))
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("date %q not understood; use YYYY-MM-DD or YYYY-MM-DD HH:MM", s)
}

func (store *MessageStore) ListChats(query string, groupsOnly bool, limit, offset int) ([]Chat, error) {
	where, args := []string{"1=1"}, []interface{}{}
	if query != "" {
		where = append(where, "(LOWER(name) LIKE LOWER(?) OR jid LIKE ?)")
		args = append(args, "%"+query+"%", "%"+query+"%")
	}
	if groupsOnly {
		where = append(where, "jid LIKE '%@g.us'")
	}
	args = append(args, limit, offset)
	rows, err := store.db.Query("SELECT jid, COALESCE(name, ''), last_message_time FROM chats WHERE "+
		strings.Join(where, " AND ")+" ORDER BY last_message_time DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chats := []Chat{}
	for rows.Next() {
		var c Chat
		var t sql.NullTime
		if err := rows.Scan(&c.JID, &c.Name, &t); err != nil {
			return nil, err
		}
		c.IsGroup = strings.HasSuffix(c.JID, "@g.us")
		c.LastMessage = showTime(t)
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

type MsgFilter struct {
	ChatJID, Sender, Query, MediaType string
	After, Before                     time.Time
	MediaOnly, OnlyUnsaved            bool
	Limit, Offset                     int
}

const msgColumns = `m.id, m.chat_jid, COALESCE(c.name, ''), m.timestamp, COALESCE(NULLIF(m.sender_name, ''), m.sender, ''),
	COALESCE(m.sender, ''), m.is_from_me, COALESCE(m.content, ''), COALESCE(m.media_type, ''), COALESCE(m.filename, ''),
	COALESCE(m.file_length, 0),
	COALESCE((SELECT group_concat(s.saved_path, ' | ') FROM saved_files s WHERE s.message_id = m.id AND s.chat_jid = m.chat_jid), '')`

func scanMsgs(rows *sql.Rows) ([]Msg, error) {
	defer rows.Close()
	msgs := []Msg{}
	for rows.Next() {
		var m Msg
		var t sql.NullTime
		var size int64
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.ChatName, &t, &m.Sender, &m.Number, &m.FromMe,
			&m.Text, &m.MediaType, &m.Filename, &size, &m.SavedTo); err != nil {
			return nil, err
		}
		m.Time = showTime(t)
		if m.MediaType != "" {
			m.SizeKB = (size + 1023) / 1024
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (store *MessageStore) ListMessages(f MsgFilter) ([]Msg, error) {
	where, args := []string{"1=1"}, []interface{}{}
	if f.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, f.ChatJID)
	}
	if f.Sender != "" {
		where = append(where, "(m.sender LIKE ? OR LOWER(m.sender_name) LIKE LOWER(?))")
		args = append(args, "%"+f.Sender+"%", "%"+f.Sender+"%")
	}
	if f.Query != "" {
		where = append(where, "(LOWER(m.content) LIKE LOWER(?) OR LOWER(m.filename) LIKE LOWER(?))")
		args = append(args, "%"+f.Query+"%", "%"+f.Query+"%")
	}
	if !f.After.IsZero() {
		where = append(where, "m.timestamp >= ?")
		args = append(args, f.After)
	}
	if !f.Before.IsZero() {
		where = append(where, "m.timestamp < ?")
		args = append(args, f.Before)
	}
	if f.MediaOnly {
		where = append(where, "m.media_type <> ''")
	}
	if f.MediaType != "" {
		where = append(where, "m.media_type = ?")
		args = append(args, f.MediaType)
	}
	if f.OnlyUnsaved {
		where = append(where, "NOT EXISTS (SELECT 1 FROM saved_files s WHERE s.message_id = m.id AND s.chat_jid = m.chat_jid)")
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := store.db.Query("SELECT "+msgColumns+" FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid WHERE "+
		strings.Join(where, " AND ")+" ORDER BY m.timestamp DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	return scanMsgs(rows)
}

// MessageContext returns the message plus up to n messages before and after it in the same chat.
func (store *MessageStore) MessageContext(messageID string, before, after int) (map[string][]Msg, error) {
	rows, err := store.db.Query("SELECT "+msgColumns+" FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid WHERE m.id = ? LIMIT 1", messageID)
	if err != nil {
		return nil, err
	}
	target, err := scanMsgs(rows)
	if err != nil {
		return nil, err
	}
	if len(target) == 0 {
		return nil, fmt.Errorf("message %s not found", messageID)
	}
	var ts sql.NullTime
	store.db.QueryRow("SELECT timestamp FROM messages WHERE id = ? AND chat_jid = ?", messageID, target[0].ChatJID).Scan(&ts)
	query := func(op, order string, n int) ([]Msg, error) {
		rows, err := store.db.Query("SELECT "+msgColumns+" FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid "+
			"WHERE m.chat_jid = ? AND m.timestamp "+op+" ? ORDER BY m.timestamp "+order+" LIMIT ?", target[0].ChatJID, ts.Time, n)
		if err != nil {
			return nil, err
		}
		return scanMsgs(rows)
	}
	b, err := query("<", "DESC", before)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	a, err := query(">", "ASC", after)
	if err != nil {
		return nil, err
	}
	return map[string][]Msg{"before": b, "message": target, "after": a}, nil
}

type Contact struct {
	Name   string `json:"name"`
	Number string `json:"number"`
}

func (store *MessageStore) SearchContacts(query string, limit int) ([]Contact, error) {
	like := "%" + query + "%"
	rows, err := store.db.Query(`
		SELECT name, number FROM (
			SELECT COALESCE(name, '') AS name, substr(jid, 1, instr(jid, '@') - 1) AS number
			FROM chats WHERE jid LIKE '%@s.whatsapp.net'
			UNION
			SELECT COALESCE(sender_name, '') AS name, sender AS number
			FROM messages WHERE is_from_me = 0 AND sender <> ''
		)
		WHERE LOWER(name) LIKE LOWER(?) OR number LIKE ?
		GROUP BY number ORDER BY name LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts := []Contact{}
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.Name, &c.Number); err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

type Totals struct {
	Newest    string `json:"newest_message_at"`
	Messages  int64  `json:"messages_stored"`
	Documents int64  `json:"documents_stored"`
	Saved     int64  `json:"files_saved_to_folders"`
}

func (store *MessageStore) Totals() Totals {
	var t Totals
	store.db.QueryRow("SELECT count(*), COALESCE(sum(media_type = 'document'), 0) FROM messages").Scan(&t.Messages, &t.Documents)
	store.db.QueryRow("SELECT count(*) FROM saved_files").Scan(&t.Saved)
	// The newest time is read through an ordered row, so it keeps the column's timestamp type
	var newest sql.NullTime
	store.db.QueryRow("SELECT timestamp FROM messages ORDER BY timestamp DESC LIMIT 1").Scan(&newest)
	t.Newest = showTime(newest)
	return t
}

// ---- Saved-files record ----

// FindSaved returns the earlier copy of this message's file in folder, if it still exists.
func (store *MessageStore) FindSaved(messageID, chatJID, folder string) string {
	rows, err := store.db.Query("SELECT saved_path FROM saved_files WHERE message_id = ? AND chat_jid = ?", messageID, chatJID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	want, _ := filepath.Abs(folder)
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && strings.EqualFold(filepath.Dir(p), want) {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func (store *MessageStore) RecordSaved(messageID, chatJID, path string) error {
	_, err := store.db.Exec("INSERT OR REPLACE INTO saved_files VALUES (?, ?, ?, ?)",
		messageID, chatJID, path, time.Now().Format("2006-01-02 15:04:05"))
	return err
}

// UpdateSavedPath keeps the record right after a saved file is renamed.
func (store *MessageStore) UpdateSavedPath(oldPath, newPath string) (int64, error) {
	res, err := store.db.Exec("UPDATE saved_files SET saved_path = ? WHERE saved_path = ?", newPath, oldPath)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
