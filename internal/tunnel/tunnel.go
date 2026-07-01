// Package tunnel embeds the 3-D Secure challenge server inside the MCP process
// and exposes it publicly via ngrok, so no external server or tunnel has to be
// run by hand. On first use it reuses a running ngrok agent's tunnel if there
// is one, otherwise it spawns `ngrok http <port>` itself.
package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ConfirmFunc finalises a payment once the shopper completes 3DS. It is
// provided by the app layer (which holds the authenticated client).
type ConfirmFunc func(ctx context.Context, orderUUID, transactionUUID, store string, details map[string]any) (any, error)

type record struct {
	Action map[string]any
	Order  string
	Txn    string
	Store  string
	Amount float64

	mu     sync.Mutex
	Result any
	Done   bool
}

// Manager lazily brings up the embedded server + ngrok tunnel, once.
type Manager struct {
	clientKey string
	confirm   ConfirmFunc

	mu        sync.Mutex
	started   bool
	publicURL string
	ngrokCmd  *exec.Cmd

	recsMu sync.Mutex
	recs   map[string]*record
	seq    int
}

func New(clientKey string, confirm ConfirmFunc) *Manager {
	return &Manager{clientKey: clientKey, confirm: confirm, recs: map[string]*record{}}
}

// Prepare stores a pending challenge and returns its id + public URLs, bringing
// the server/tunnel up on first call.
func (m *Manager) Prepare(ctx context.Context, rec RecordInput) (map[string]any, error) {
	base, err := m.ensure(ctx)
	if err != nil {
		return nil, err
	}
	m.recsMu.Lock()
	m.seq++
	id := "c" + strconv.Itoa(m.seq) + "-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
	m.recs[id] = &record{Action: rec.Action, Order: rec.OrderUUID, Txn: rec.TransactionUUID, Store: rec.Store, Amount: rec.Amount}
	m.recsMu.Unlock()
	return map[string]any{
		"id":         id,
		"pay_url":    base + "/pay/" + id,
		"status_url": base + "/status/" + id,
	}, nil
}

// Reserve creates a pending record and returns its id plus the returnUrl Adyen
// should send the shopper back to after a redirect 3DS challenge. Call Fill
// once the payment is initiated to attach the action + transaction UUID.
func (m *Manager) Reserve(ctx context.Context, order, store string) (id, returnURL string, err error) {
	base, err := m.ensure(ctx)
	if err != nil {
		return "", "", err
	}
	m.recsMu.Lock()
	m.seq++
	id = "r" + strconv.Itoa(m.seq) + "-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
	m.recs[id] = &record{Order: order, Store: store}
	m.recsMu.Unlock()
	return id, base + "/return/" + id, nil
}

// Fill attaches the action, transaction UUID and amount to a reserved record.
func (m *Manager) Fill(id string, action map[string]any, txn string, amount float64) {
	m.recsMu.Lock()
	if r := m.recs[id]; r != nil {
		r.Action = action
		r.Txn = txn
		r.Amount = amount
	}
	m.recsMu.Unlock()
}

// PayURL returns the challenge-page URL for a reserved id.
func (m *Manager) PayURL(id string) string {
	return m.publicURL + "/pay/" + id
}

// RecordInput is the data needed to prepare a challenge.
type RecordInput struct {
	Action          map[string]any
	OrderUUID       string
	TransactionUUID string
	Store           string
	Amount          float64
}

func (m *Manager) ensure(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return m.publicURL, nil
	}

	publicURL, port, err := discoverTunnel()
	if err == nil {
		// Reuse a running agent's tunnel; serve on the port it forwards to.
		if serveErr := m.serve(port); serveErr != nil {
			return "", fmt.Errorf("a tunnel to :%d exists but that port is busy (%v); stop whatever is on it or the other ngrok agent", port, serveErr)
		}
		m.publicURL, m.started = publicURL, true
		return publicURL, nil
	}

	// No agent running — pick a free port, serve, and spawn ngrok for it.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		return "", lerr
	}
	p := ln.Addr().(*net.TCPAddr).Port
	go http.Serve(ln, m.mux())

	if _, err := exec.LookPath("ngrok"); err != nil {
		return "", fmt.Errorf("no ngrok agent running and ngrok not installed: %w", err)
	}
	cmd := exec.Command("ngrok", "http", strconv.Itoa(p), "--log", "stdout")
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start ngrok: %w", err)
	}
	m.ngrokCmd = cmd

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if u, tp, e := discoverTunnel(); e == nil && tp == p {
			m.publicURL, m.started = u, true
			return u, nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return "", fmt.Errorf("ngrok started but no public URL appeared (free tier allows one agent — is another running?)")
}

// serve starts the embedded HTTP server on the given port.
func (m *Manager) serve(port int) error {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	go http.Serve(ln, m.mux())
	return nil
}

// discoverTunnel returns the public URL and forward port of a running ngrok
// agent's first https tunnel, via its local API.
func discoverTunnel() (string, int, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:4040/api/tunnels")
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Tunnels []struct {
			PublicURL string `json:"public_url"`
			Config    struct {
				Addr string `json:"addr"`
			} `json:"config"`
		} `json:"tunnels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	for _, t := range out.Tunnels {
		if !strings.HasPrefix(t.PublicURL, "https://") {
			continue
		}
		addr := t.Config.Addr
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			if p, e := strconv.Atoi(addr[i+1:]); e == nil {
				return t.PublicURL, p, nil
			}
		}
	}
	return "", 0, fmt.Errorf("no https tunnel found")
}
