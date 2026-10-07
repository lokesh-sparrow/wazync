package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			mediaURL(img.GetURL(), img.GetDirectPath()), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			mediaURL(vid.GetURL(), vid.GetDirectPath()), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			mediaURL(aud.GetURL(), aud.GetDirectPath()), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			mediaURL(doc.GetURL(), doc.GetDirectPath()), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// mediaURL prefers the full media URL and falls back to the direct path
func mediaURL(url, directPath string) string {
	if url != "" {
		return url
	}
	return directPath
}

// resolveSender returns the sender's phone number (resolving WhatsApp privacy
// ids where possible) and the best known display name.
func resolveSender(client *whatsmeow.Client, sender, senderAlt types.JID, pushName string, isFromMe bool) (string, string) {
	if isFromMe {
		if client.Store.ID != nil {
			return client.Store.ID.User, "Me"
		}
		return sender.User, "Me"
	}
	ctx := context.Background()
	pn := sender
	if sender.Server == types.HiddenUserServer {
		if senderAlt.Server == types.DefaultUserServer {
			pn = senderAlt
		} else if found, err := client.Store.LIDs.GetPNForLID(ctx, sender.ToNonAD()); err == nil && !found.IsEmpty() {
			pn = found
		}
	}
	return pn.User, contactName(client, pushName, pn.ToNonAD(), sender.ToNonAD())
}

// contactName prefers the phone's address book, then the sender's own WhatsApp name.
func contactName(client *whatsmeow.Client, pushName string, jids ...types.JID) string {
	ctx := context.Background()
	for _, jid := range jids {
		if jid.IsEmpty() {
			continue
		}
		if c, err := client.Store.Contacts.GetContact(ctx, jid); err == nil && c.Found {
			for _, name := range []string{c.FullName, c.FirstName, c.BusinessName, c.PushName} {
				if name != "" {
					return name
				}
			}
		}
	}
	return pushName
}

// backfillSenders resolves privacy ids and names for stored messages that lack them.
// The id mapping grows as WhatsApp shares it, so this runs periodically.
func backfillSenders(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	rows, err := store.db.Query("SELECT DISTINCT sender FROM messages WHERE is_from_me = 0 AND (sender_name IS NULL OR sender_name = '' OR sender_name = sender)")
	if err != nil {
		logger.Warnf("Sender backfill query failed: %v", err)
		return
	}
	var senders []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil && s != "" {
			senders = append(senders, s)
		}
	}
	rows.Close()

	ctx := context.Background()
	updated := 0
	for _, s := range senders {
		lid := types.NewJID(s, types.HiddenUserServer)
		pn := types.NewJID(s, types.DefaultUserServer)
		if found, err := client.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !found.IsEmpty() {
			pn = found.ToNonAD()
		} else {
			lid = types.EmptyJID
		}
		name := contactName(client, "", pn, lid)
		if name == "" {
			if pn.User == s {
				continue
			}
			name = pn.User
		}
		if _, err := store.db.Exec("UPDATE messages SET sender = ?, sender_name = ? WHERE sender = ? AND is_from_me = 0", pn.User, name, s); err == nil {
			updated++
		}
	}
	store.db.Exec("UPDATE messages SET sender_name = 'Me' WHERE is_from_me = 1 AND (sender_name IS NULL OR sender_name = '')")
	logger.Infof("Sender backfill: resolved %d of %d senders", updated, len(senders))
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender, senderName := resolveSender(client, msg.Info.Sender, msg.Info.SenderAlt, msg.Info.PushName, msg.Info.IsFromMe)
	lastMessageAt = time.Now()

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		senderName,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log files only, never message text
		if mediaType != "" {
			fmt.Printf("[%s] %s %s received\n", timestamp, direction, mediaType)
		}
	}
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	// Keep the query string: whatsmeow appends "&hash=..." to the direct path
	pathPart := parts[1]

	// Create proper direct path format
	return "/" + pathPart
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Debugf("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Debugf("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Debugf("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Debugf("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Debugf("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				senderJID := jid
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					// Group messages name the sender in the key or, in history sync, on the message itself
					participant := msg.Message.Key.GetParticipant()
					if participant == "" {
						participant = msg.Message.GetParticipant()
					}
					if !isFromMe && participant != "" {
						if p, err := types.ParseJID(participant); err == nil {
							senderJID = p
						}
					}
				}
				sender, senderName := resolveSender(client, senderJID, types.EmptyJID, msg.Message.GetPushName(), isFromMe)

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					senderName,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}
