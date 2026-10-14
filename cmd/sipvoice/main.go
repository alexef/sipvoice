// Command sipvoice places outbound AI voice calls over a plain SIP account.
//
// It never sends REGISTER (other phones on the account keep receiving
// calls) and declines every inbound INVITE.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/alexef/sipvoice/internal/config"
	"github.com/alexef/sipvoice/internal/daemon"
	"github.com/alexef/sipvoice/internal/sip"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `sipvoice %s

usage: sipvoice [--config FILE] <command> [args]

commands:
  daemon                               run the call daemon and local HTTP API
  call --to NUMBER --instructions TEXT [--lang en] [--max-seconds N]
  status [CALL_ID]                     daemon status or one call
  hangup CALL_ID                       end a call
  probe                                send SIP OPTIONS (no call, no REGISTER)
  version

default config: %s
`, version, config.DefaultPath())
	os.Exit(2)
}

func api(cfg *config.Config, method, path string, body any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+cfg.Listen+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func main() {
	global := flag.NewFlagSet("sipvoice", flag.ExitOnError)
	cfgPath := global.String("config", config.DefaultPath(), "config file")
	global.Usage = usage
	_ = global.Parse(os.Args[1:])
	args := global.Args()
	if len(args) < 1 {
		usage()
	}
	explicit := false
	global.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
	cfg, err := config.Load(*cfgPath, explicit)
	if err != nil {
		fail(err)
	}
	switch args[0] {
	case "daemon":
		if err := daemon.New(cfg).Serve(); err != nil {
			fail(err)
		}
	case "call":
		fs := flag.NewFlagSet("call", flag.ExitOnError)
		to := fs.String("to", "", "number to call (E.164, must be in allowlist)")
		ins := fs.String("instructions", "", "what the agent should do on the call")
		lang := fs.String("lang", "en", "language code, e.g. en, de, fr")
		max := fs.Int("max-seconds", 0, "max call duration (capped by limits.max_call_seconds)")
		_ = fs.Parse(args[1:])
		api(cfg, "POST", "/calls", daemon.CallRequest{To: *to, Instructions: *ins, Lang: *lang, MaxSeconds: *max})
	case "status":
		if len(args) > 1 {
			api(cfg, "GET", "/calls/"+args[1], nil)
		} else {
			api(cfg, "GET", "/status", nil)
		}
	case "hangup":
		if len(args) < 2 {
			usage()
		}
		api(cfg, "POST", "/calls/"+args[1]+"/hangup", nil)
	case "probe":
		if cfg.SIP.Domain == "" {
			fail(fmt.Errorf("sip.domain is not set"))
		}
		s, err := sip.NewClient(daemon.SIPConfig(cfg), func(string, ...any) {})
		if err != nil {
			fail(err)
		}
		r, err := s.Probe()
		if err != nil {
			fail(err)
		}
		ip, port := s.PublicAddr()
		fmt.Printf("server replied: %s | public addr: %s:%d\n", r, ip, port)
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}
