// Package daemon runs calls and exposes the local HTTP API.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexef/sipvoice/internal/config"
	"github.com/alexef/sipvoice/internal/sip"
)

// New creates a daemon for cfg.
func New(cfg *config.Config) *Daemon { return &Daemon{cfg: cfg, calls: map[string]*Call{}} }

// SIPConfig maps the file config to the SIP client config.
func SIPConfig(c *config.Config) *sip.Config {
	return &sip.Config{User: c.SIP.Username, AuthUser: c.SIP.AuthUsername, Password: c.SIP.Password,
		Domain: c.SIP.Domain, Proxy: c.SIP.Proxy, Port: c.SIP.Port, LocalPort: c.SIP.LocalPort, UserAgent: "sipvoice"}
}

// Allowed reports whether number is in the allowlist (formatting ignored).
func Allowed(list []string, number string) bool {
	n := NormalizeNumber(number)
	if n == "" {
		return false
	}
	for _, a := range list {
		if NormalizeNumber(a) == n {
			return true
		}
	}
	return false
}

func randHex(n int) string { b := make([]byte, n); rand.Read(b); return hex.EncodeToString(b) }

type Daemon struct {
	cfg    *config.Config
	sip    *sip.Client
	mu     sync.Mutex
	active *Call
	calls  map[string]*Call
}

func (d *Daemon) logf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(f, a...))
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// StartCall validates the request and places the call in the background.
func (d *Daemon) StartCall(req CallRequest) (*Call, error) {
	if req.To == "" || strings.TrimSpace(req.Instructions) == "" {
		return nil, fmt.Errorf("to and instructions are required")
	}
	if req.Lang == "" {
		req.Lang = "en"
	}
	if !Allowed(d.cfg.Allowlist, req.To) {
		return nil, fmt.Errorf("number is not in the allowlist")
	}
	if req.MaxSeconds <= 0 || req.MaxSeconds > d.cfg.Limits.MaxCallSeconds {
		req.MaxSeconds = d.cfg.Limits.MaxCallSeconds
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		return nil, fmt.Errorf("a call is already active: %s", d.active.ID)
	}
	id := time.Now().Format("20060102-150405") + "-" + randHex(2)
	c := &Call{ID: id, Req: req, State: "starting", Started: time.Now(), hangup: make(chan struct{}), dir: filepath.Join(d.cfg.CallsDir, id)}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, err
	}
	c.logf, _ = os.Create(filepath.Join(c.dir, "call.log"))
	c.log("call to %s lang=%s max=%ds", req.To, req.Lang, req.MaxSeconds)
	d.active = c
	d.calls[id] = c
	go d.runCall(c)
	return c, nil
}

// Serve runs the local HTTP API until a signal or fatal error.
func (d *Daemon) Serve() error {
	if err := d.cfg.Validate(); err != nil {
		return err
	}
	s, err := sip.NewClient(SIPConfig(d.cfg), d.logf)
	if err != nil {
		return err
	}
	d.sip = s
	mux := http.NewServeMux()
	mux.HandleFunc("POST /calls", func(w http.ResponseWriter, r *http.Request) {
		var req CallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		c, err := d.StartCall(req)
		if err != nil {
			jsonOut(w, 409, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 201, map[string]string{"id": c.ID, "dir": c.dir})
	})
	mux.HandleFunc("GET /calls/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		c := d.calls[r.PathValue("id")]
		d.mu.Unlock()
		if c == nil {
			jsonOut(w, 404, map[string]string{"error": "not found"})
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		jsonOut(w, 200, c)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		st := map[string]any{"sip_server": d.cfg.SIP.Domain, "registered": false, "note": "outbound-only, never registers"}
		if d.active != nil {
			st["active"] = map[string]string{"id": d.active.ID, "state": d.active.State}
		}
		jsonOut(w, 200, st)
	})
	mux.HandleFunc("POST /calls/{id}/hangup", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		c := d.calls[r.PathValue("id")]
		d.mu.Unlock()
		if c == nil {
			jsonOut(w, 404, map[string]string{"error": "not found"})
			return
		}
		c.Hangup("hangup via API")
		jsonOut(w, 200, map[string]string{"ok": "hangup requested"})
	})
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		sg := <-sigc
		d.shutdown("signal " + sg.String())
		os.Exit(1)
	}()
	d.logf("sipvoice listening on %s (server %s, outbound only)", d.cfg.Listen, d.cfg.SIP.Domain)
	srv := &http.Server{Addr: d.cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	err = srv.ListenAndServe()
	d.shutdown("fatal: " + err.Error())
	return err
}

// shutdown hangs up an active call (BYE, or CANCEL while ringing) before exit.
func (d *Daemon) shutdown(why string) {
	d.mu.Lock()
	c := d.active
	d.mu.Unlock()
	if c == nil {
		return
	}
	d.logf("shutdown (%s): hanging up %s", why, c.ID)
	c.Hangup(why)
	d.sendBye(c)
	time.Sleep(1500 * time.Millisecond) // let CANCEL/BYE go out
}
