// Wazync bridge: links to WhatsApp as a linked device, keeps a local record of
// chats and serves it to the Wazync Claude extension on 127.0.0.1 only.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	sqlite "modernc.org/sqlite"
	"rsc.io/qr"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// version is set at build time with -ldflags "-X main.version=..."
var version = "dev"

const defaultPort = 47823

var (
	startedAt     = time.Now()
	lastMessageAt time.Time
	installedFrom string // the extension folder, so a sign-in start can tell when it is uninstalled
)

func autostartArgs() string {
	return fmt.Sprintf("--extension-dir \"%s\" --autostart true", installedFrom)
}

func init() {
	sql.Register("sqlite3", &sqlite.Driver{})
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

func main() {
	dataDir := flag.String("data", filepath.Join(os.Getenv("LOCALAPPDATA"), "Wazync"), "folder for the WhatsApp link, message record and log")
	port := flag.Int("port", defaultPort, "local port for the Wazync extension")
	extensionDir := flag.String("extension-dir", "", "folder of the installed Claude extension; the bridge stops when it is removed")
	autostart := flag.String("autostart", "", "\"true\" or \"false\": start the bridge when you sign in to Windows")
	flag.Parse()

	if err := os.MkdirAll(filepath.Join(*dataDir, "store"), 0700); err != nil {
		os.Exit(1)
	}
	os.Chdir(*dataDir)
	if f, err := os.Create("bridge.log"); err == nil {
		os.Stdout, os.Stderr = f, f
	}

	// Uninstalled extension: remove our sign-in entry and stop.
	if *extensionDir != "" {
		if _, err := os.Stat(*extensionDir); os.IsNotExist(err) {
			setAutostart(false, "")
			fmt.Println("Wazync extension removed; bridge stopped and sign-in start removed.")
			return
		}
	}

	// One bridge at a time: holding the port is the lock.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Println("Another Wazync bridge is already running.")
		return
	}

	installedFrom = *extensionDir
	if *autostart != "" {
		if err := setAutostart(*autostart == "true", autostartArgs()); err != nil {
			fmt.Printf("Could not update sign-in start: %v\n", err)
		}
	}

	logger := waLog.Stdout("Client", "INFO", false)
	logger.Infof("Wazync bridge %s starting", version)

	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:store/whatsapp.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)", waLog.Stdout("Database", "WARN", false))
	if err != nil {
		logger.Errorf("Failed to open the WhatsApp link database: %v", err)
		return
	}
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		logger.Errorf("Failed to read the WhatsApp link: %v", err)
		return
	}
	client := whatsmeow.NewClient(deviceStore, logger)

	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to open the message record: %v", err)
		return
	}
	defer messageStore.Close()

	b := &Bridge{client: client, store: messageStore, logger: logger, quit: make(chan struct{})}

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, messageStore, v, logger)
		case *events.HistorySync:
			handleHistorySync(client, messageStore, v, logger)
		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
		case *events.LoggedOut:
			// The phone removed this linked device. Stop; the next start shows a new QR code.
			logger.Warnf("Unlinked from WhatsApp; a new QR code is needed")
			b.Stop()
		}
	})

	go serveAPI(listener, b)

	if client.Store.ID == nil {
		b.StartPairing()
	} else if err := client.Connect(); err != nil {
		logger.Errorf("Failed to connect: %v", err)
	}

	go func() {
		time.Sleep(30 * time.Second)
		for {
			if client.IsLoggedIn() {
				backfillSenders(client, messageStore, logger)
			}
			time.Sleep(30 * time.Minute)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
	case <-b.quit:
	}
	client.Disconnect()
	fmt.Println("Bridge stopped.")
}

// Bridge holds the running WhatsApp client and the pairing state.
type Bridge struct {
	client *whatsmeow.Client
	store  *MessageStore
	logger waLog.Logger

	mu      sync.Mutex
	pairing bool
	qrPNG   []byte

	quit     chan struct{}
	quitOnce sync.Once
}

func (b *Bridge) Stop() {
	b.quitOnce.Do(func() { close(b.quit) })
}

// StartPairing shows QR codes until the phone links this device or the codes run out.
func (b *Bridge) StartPairing() {
	b.mu.Lock()
	if b.pairing || b.client.Store.ID != nil {
		b.mu.Unlock()
		return
	}
	b.pairing = true
	b.qrPNG = nil
	b.mu.Unlock()

	if b.client.IsConnected() {
		b.client.Disconnect()
	}
	qrChan, err := b.client.GetQRChannel(context.Background())
	if err == nil {
		err = b.client.Connect()
	}
	if err != nil {
		b.logger.Errorf("Failed to start linking: %v", err)
		b.mu.Lock()
		b.pairing = false
		b.mu.Unlock()
		return
	}
	go func() {
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				if code, err := qr.Encode(evt.Code, qr.L); err == nil {
					code.Scale = 8
					png := code.PNG()
					b.mu.Lock()
					b.qrPNG = png
					b.mu.Unlock()
					os.WriteFile("store/qr.png", png, 0600)
				}
			case "success":
				b.logger.Infof("Linked to WhatsApp")
			default:
				b.logger.Infof("Linking ended: %s", evt.Event)
			}
		}
		os.Remove("store/qr.png")
		b.mu.Lock()
		b.pairing = false
		b.qrPNG = nil
		b.mu.Unlock()
		if b.client.Store.ID == nil {
			// Codes expired without a scan; disconnect until linking is asked for again.
			b.client.Disconnect()
		}
	}()
}

// CurrentQR returns the latest QR code image, waiting briefly for the first one.
func (b *Bridge) CurrentQR(wait time.Duration) []byte {
	deadline := time.Now().Add(wait)
	for {
		b.mu.Lock()
		png := b.qrPNG
		b.mu.Unlock()
		if png != nil || time.Now().After(deadline) || b.client.Store.ID != nil {
			return png
		}
		time.Sleep(250 * time.Millisecond)
	}
}
