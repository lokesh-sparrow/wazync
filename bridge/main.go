// Wazync bridge: links to WhatsApp as a linked device, keeps a local record of
// chats and serves it to the Wazync Claude extension on 127.0.0.1 only.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

// connectionFile tells the extension where the bridge listens and which key to send.
const connectionFile = "bridge.json"

func writeConnectionFile(port int) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	token := hex.EncodeToString(key)
	data, _ := json.Marshal(map[string]interface{}{"version": version, "port": port, "token": token, "pid": os.Getpid()})
	if err := os.WriteFile(connectionFile+".tmp", data, 0600); err != nil {
		return "", err
	}
	return token, os.Rename(connectionFile+".tmp", connectionFile)
}

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

// defaultDataDir is %USERPROFILE%\.wazync. It is deliberately outside AppData:
// the Microsoft Store edition of Claude Desktop redirects its extensions' AppData
// into a private folder that the bridge, which runs outside the app, cannot see.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("USERPROFILE")
	}
	return filepath.Join(home, ".wazync")
}

// extensionInstalled checks the extension folder where Claude Desktop shows it and,
// for the Microsoft Store edition, where Windows really keeps it
// (%LOCALAPPDATA%\Packages\<app>\LocalCache\Roaming\...).
func extensionInstalled(dir string) bool {
	if _, err := os.Stat(dir); err == nil {
		return true
	}
	roaming := os.Getenv("APPDATA")
	rel, err := filepath.Rel(roaming, dir)
	if roaming == "" || err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	packages, _ := filepath.Glob(filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", "*", "LocalCache", "Roaming"))
	for _, p := range packages {
		if _, err := os.Stat(filepath.Join(p, rel)); err == nil {
			return true
		}
	}
	return false
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

func main() {
	dataDir := flag.String("data", defaultDataDir(), "folder for the WhatsApp link, message record and log")
	port := flag.Int("port", 0, "local port for the Wazync extension; 0 picks a free one")
	extensionDir := flag.String("extension-dir", "", "folder of the installed Claude extension; the bridge stops when it is removed")
	autostart := flag.String("autostart", "", "\"true\" or \"false\": start the bridge when you sign in to Windows")
	flag.Parse()

	if err := os.MkdirAll(filepath.Join(*dataDir, "store"), 0700); err != nil {
		os.Exit(1)
	}
	os.Chdir(*dataDir)

	// Uninstalled extension: remove our sign-in entry and stop.
	if *extensionDir != "" && !extensionInstalled(*extensionDir) {
		setAutostart(false, "")
		return
	}

	// One bridge per Windows user.
	if err := lockInstance("bridge.lock"); err != nil {
		return
	}
	if f, err := os.Create("bridge.log"); err == nil {
		os.Stdout, os.Stderr = f, f
	}

	// A free local port of our own, so several users on one computer never clash.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Printf("Cannot open the local port: %v\n", err)
		return
	}
	// A new secret key every start. Only this Windows user can read the file, and
	// the bridge refuses every request that does not carry the key.
	token, err := writeConnectionFile(listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		fmt.Printf("Cannot write the connection file: %v\n", err)
		return
	}
	defer os.Remove(connectionFile)

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

	go serveAPI(listener, b, token)

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
