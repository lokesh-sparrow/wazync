package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
	"go.mau.fi/whatsmeow"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...interface{}) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n < 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func boolParam(r *http.Request, name string) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return v
}

func serveAPI(listener net.Listener, b *Bridge, token string) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		pairing := b.pairing
		b.mu.Unlock()
		writeJSON(w, 200, map[string]interface{}{
			"version":              version,
			"linked":               b.client.Store.ID != nil,
			"connected":            b.client.IsConnected(),
			"linking_in_progress":  pairing,
			"bridge_started_at":    formatTime(startedAt),
			"last_live_message_at": formatTime(lastMessageAt),
			"totals":               b.store.Totals(),
		})
	})

	mux.HandleFunc("POST /api/link", func(w http.ResponseWriter, r *http.Request) {
		if b.client.Store.ID != nil {
			writeJSON(w, 200, map[string]interface{}{"linked": true})
			return
		}
		b.StartPairing()
		png := b.CurrentQR(20 * time.Second)
		if png == nil {
			fail(w, 503, "WhatsApp did not send a QR code yet; check the internet connection and try again")
			return
		}
		writeJSON(w, 200, map[string]interface{}{"linked": false, "qr_png_base64": base64.StdEncoding.EncodeToString(png)})
	})

	mux.HandleFunc("GET /api/chats", func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 30, 200)
		chats, err := b.store.ListChats(r.URL.Query().Get("query"), boolParam(r, "groups_only"), limit, intParam(r, "page", 0, 10000)*limit)
		if err != nil {
			fail(w, 500, "%v", err)
			return
		}
		writeJSON(w, 200, chats)
	})

	mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := MsgFilter{
			ChatJID: q.Get("chat_jid"), Sender: q.Get("sender"), Query: q.Get("query"), MediaType: q.Get("media_type"),
			MediaOnly: boolParam(r, "media_only"), OnlyUnsaved: boolParam(r, "only_unsaved"),
			Limit: intParam(r, "limit", 50, 200),
		}
		f.Offset = intParam(r, "page", 0, 10000) * f.Limit
		for name, dst := range map[string]*time.Time{"after": &f.After, "before": &f.Before} {
			if s := q.Get(name); s != "" {
				t, err := parseWhen(s)
				if err != nil {
					fail(w, 400, "%v", err)
					return
				}
				*dst = t
			}
		}
		msgs, err := b.store.ListMessages(f)
		if err != nil {
			fail(w, 500, "%v", err)
			return
		}
		writeJSON(w, 200, msgs)
	})

	mux.HandleFunc("GET /api/context", func(w http.ResponseWriter, r *http.Request) {
		ctx, err := b.store.MessageContext(r.URL.Query().Get("message_id"), intParam(r, "before", 5, 50), intParam(r, "after", 5, 50))
		if err != nil {
			fail(w, 404, "%v", err)
			return
		}
		writeJSON(w, 200, ctx)
	})

	mux.HandleFunc("GET /api/contacts", func(w http.ResponseWriter, r *http.Request) {
		contacts, err := b.store.SearchContacts(r.URL.Query().Get("query"), intParam(r, "limit", 30, 200))
		if err != nil {
			fail(w, 500, "%v", err)
			return
		}
		writeJSON(w, 200, contacts)
	})

	mux.HandleFunc("POST /api/save", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MessageID string `json:"message_id"`
			ChatJID   string `json:"chat_jid"`
			Folder    string `json:"folder"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MessageID == "" || req.ChatJID == "" || req.Folder == "" {
			fail(w, 400, "message_id, chat_jid and folder are required")
			return
		}
		if !filepath.IsAbs(req.Folder) {
			fail(w, 400, "folder must be a full path, such as D:\\Clients\\ACME")
			return
		}
		if earlier := b.store.FindSaved(req.MessageID, req.ChatJID, req.Folder); earlier != "" {
			writeJSON(w, 200, saveResult(earlier, true))
			return
		}
		path, err := saveMedia(b, req.MessageID, req.ChatJID, req.Folder)
		if err != nil {
			fail(w, 502, "%v", err)
			return
		}
		b.store.RecordSaved(req.MessageID, req.ChatJID, path)
		writeJSON(w, 200, saveResult(path, false))
	})

	mux.HandleFunc("POST /api/rename", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path    string `json:"path"`
			NewName string `json:"new_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || req.NewName == "" {
			fail(w, 400, "path and new_name are required")
			return
		}
		// Keep the original extension; names like "ADCB-06.10.26-Party" contain dots of their own
		name := safeName(req.NewName)
		if ext := filepath.Ext(req.Path); ext != "" && !strings.HasSuffix(strings.ToLower(name), strings.ToLower(ext)) {
			name += ext
		}
		target := filepath.Join(filepath.Dir(req.Path), name)
		if strings.EqualFold(target, req.Path) {
			writeJSON(w, 200, map[string]string{"path": target})
			return
		}
		if _, err := os.Stat(target); err == nil {
			fail(w, 409, "%s already exists; choose another name", name)
			return
		}
		// Only files Wazync saved can be renamed.
		var known int
		b.store.db.QueryRow("SELECT count(*) FROM saved_files WHERE saved_path = ?", req.Path).Scan(&known)
		if known == 0 {
			fail(w, 403, "only files saved by Wazync can be renamed")
			return
		}
		if err := os.Rename(req.Path, target); err != nil {
			fail(w, 500, "%v", err)
			return
		}
		b.store.UpdateSavedPath(req.Path, target)
		writeJSON(w, 200, map[string]string{"path": target})
	})

	mux.HandleFunc("POST /api/autostart", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enable       bool   `json:"enable"`
			ExtensionDir string `json:"extension_dir"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, "enable is required")
			return
		}
		if req.ExtensionDir != "" {
			installedFrom = req.ExtensionDir
		}
		if err := setAutostart(req.Enable, autostartArgs()); err != nil {
			fail(w, 500, "%v", err)
			return
		}
		writeJSON(w, 200, map[string]bool{"start_with_windows": req.Enable})
	})

	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"stopping": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			b.Stop()
		}()
	})

	// Every request must carry the secret key from the connection file.
	want := []byte("Bearer " + token)
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			fail(w, 401, "missing or wrong key")
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: guarded, ReadHeaderTimeout: 10 * time.Second}
	srv.Serve(listener)
}

// saveResult describes a saved file and, so Claude can name it from its content,
// includes the text of a PDF or a small image.
func saveResult(path string, already bool) map[string]interface{} {
	res := map[string]interface{}{"path": path, "already_saved": already}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		if text := pdfText(path, 4000); text != "" {
			res["text"] = text
		}
	case ".jpg", ".jpeg", ".png", ".webp":
		if info, err := os.Stat(path); err == nil && info.Size() <= 1_500_000 {
			if data, err := os.ReadFile(path); err == nil {
				res["image_base64"] = base64.StdEncoding.EncodeToString(data)
				res["image_type"] = map[string]string{".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp"}[strings.ToLower(filepath.Ext(path))]
			}
		}
	}
	return res
}

func pdfText(path string, max int) string {
	f, r, err := pdf.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// Page by page, so one damaged page does not lose the rest of the document.
	var sb strings.Builder
	for i := 1; i <= r.NumPage() && sb.Len() < max; i++ {
		func() {
			defer func() { recover() }()
			p := r.Page(i)
			if p.V.IsNull() {
				return
			}
			if t, err := p.GetPlainText(nil); err == nil {
				sb.WriteString(t)
				sb.WriteString(" ")
			}
		}()
	}
	text := strings.Join(strings.Fields(sb.String()), " ")
	if len(text) > max {
		text = text[:max]
	}
	return text
}

// safeName keeps a file name from other people's messages from escaping its folder.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		name = "file"
	}
	return name
}

// uniquePath never overwrites: "name.pdf" becomes "name (1).pdf" and so on.
func uniquePath(folder, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	path := filepath.Join(folder, name)
	for n := 1; ; n++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		path = filepath.Join(folder, fmt.Sprintf("%s (%d)%s", base, n, ext))
	}
}

// saveMedia downloads a message's file from WhatsApp straight into folder.
func saveMedia(b *Bridge, messageID, chatJID, folder string) (string, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	err := b.store.db.QueryRow(
		"SELECT COALESCE(media_type, ''), COALESCE(filename, ''), COALESCE(url, ''), media_key, file_sha256, file_enc_sha256, COALESCE(file_length, 0) FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)
	if err != nil {
		return "", fmt.Errorf("message not found")
	}
	types := map[string]whatsmeow.MediaType{
		"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo,
		"audio": whatsmeow.MediaAudio, "document": whatsmeow.MediaDocument,
	}
	waType, ok := types[mediaType]
	if !ok {
		return "", fmt.Errorf("this message has no file")
	}
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return "", fmt.Errorf("this file's download details are missing")
	}
	directPath := extractDirectPathFromURL(url)
	if strings.HasPrefix(url, "/") {
		url = ""
	}
	data, err := b.client.Download(context.Background(), &MediaDownloader{
		URL: url, DirectPath: directPath, MediaKey: mediaKey, FileLength: fileLength,
		FileSHA256: fileSHA256, FileEncSHA256: fileEncSHA256, MediaType: waType,
	})
	if err != nil {
		return "", fmt.Errorf("WhatsApp no longer has this file (older files expire from its servers): %v", err)
	}
	if err := os.MkdirAll(folder, 0755); err != nil {
		return "", fmt.Errorf("cannot create folder: %v", err)
	}
	path := uniquePath(folder, safeName(filename))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("cannot write file: %v", err)
	}
	fmt.Printf("Saved a %s (%d bytes)\n", mediaType, len(data))
	return path, nil
}
