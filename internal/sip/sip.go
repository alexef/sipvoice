// Package sip is a minimal outbound-only SIP user agent client over UDP.
package sip

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds SIP account settings.
type Config struct {
	User, AuthUser, Password, Domain, Proxy string
	Port, LocalPort                         int
	UserAgent                               string
}

// Msg is a minimal parsed SIP message.
type Msg struct {
	StartLine string
	Headers   []hdr
	Body      string
	Status    int
	Method    string
}
type hdr struct{ k, v string }

func (m *Msg) Get(k string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.k, k) {
			return h.v
		}
	}
	return ""
}
func (m *Msg) All(k string) []string {
	var r []string
	for _, h := range m.Headers {
		if strings.EqualFold(h.k, k) {
			r = append(r, h.v)
		}
	}
	return r
}

var compact = map[string]string{"v": "Via", "f": "From", "t": "To", "i": "Call-ID", "m": "Contact", "l": "Content-Length", "c": "Content-Type"}

func parseSIP(b []byte) (*Msg, error) {
	s := string(b)
	parts := strings.SplitN(s, "\r\n\r\n", 2)
	lines := strings.Split(parts[0], "\r\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty")
	}
	m := &Msg{StartLine: lines[0]}
	if len(parts) == 2 {
		m.Body = parts[1]
	}
	for _, l := range lines[1:] {
		i := strings.Index(l, ":")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(l[:i])
		if c, ok := compact[strings.ToLower(k)]; ok {
			k = c
		}
		m.Headers = append(m.Headers, hdr{k, strings.TrimSpace(l[i+1:])})
	}
	if strings.HasPrefix(m.StartLine, "SIP/2.0 ") {
		f := strings.Fields(m.StartLine)
		if len(f) >= 2 {
			m.Status, _ = strconv.Atoi(f[1])
		}
	} else {
		m.Method = strings.Fields(m.StartLine)[0]
	}
	return m, nil
}

func randHex(n int) string { b := make([]byte, n); rand.Read(b); return hex.EncodeToString(b) }
func md5h(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

var paramRe = regexp.MustCompile(`(\w+)=("([^"]*)"|[^,\s]+)`)

func digest(challenge, method, uri, user, pass string, nc int) string {
	p := map[string]string{}
	for _, m := range paramRe.FindAllStringSubmatch(challenge, -1) {
		v := m[2]
		if m[3] != "" || strings.HasPrefix(v, `"`) {
			v = m[3]
		}
		p[strings.ToLower(m[1])] = v
	}
	ha1 := md5h(user + ":" + p["realm"] + ":" + pass)
	ha2 := md5h(method + ":" + uri)
	out := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5`, user, p["realm"], p["nonce"], uri)
	if strings.Contains(p["qop"], "auth") {
		cn := randHex(8)
		ncs := fmt.Sprintf("%08x", nc)
		resp := md5h(ha1 + ":" + p["nonce"] + ":" + ncs + ":" + cn + ":auth:" + ha2)
		out += fmt.Sprintf(`, response="%s", qop=auth, nc=%s, cnonce="%s"`, resp, ncs, cn)
	} else {
		out += fmt.Sprintf(`, response="%s"`, md5h(ha1+":"+p["nonce"]+":"+ha2))
	}
	if p["opaque"] != "" {
		out += fmt.Sprintf(`, opaque="%s"`, p["opaque"])
	}
	return out
}

// Client is a UDP user agent client. It NEVER sends REGISTER, so it can
// not take over inbound calls from other devices on the same account.
// Unsolicited inbound requests (INVITE etc.) are rejected with 603.
type Client struct {
	cfg      *Config
	conn     *net.UDPConn
	server   *net.UDPAddr
	localIP  string
	localPrt int
	pubIP    string
	pubPort  int
	mu       sync.Mutex
	waiters  map[string]chan *Msg // by Call-ID
	onReq    map[string]func(*Msg)
	log      func(string, ...any)
	traces   map[string]func(string, ...any) // per Call-ID SIP trace
}

func NewClient(cfg *Config, logf func(string, ...any)) (*Client, error) {
	if cfg.AuthUser == "" {
		cfg.AuthUser = cfg.User
	}
	if cfg.Proxy == "" {
		cfg.Proxy = cfg.Domain
	}
	if cfg.Port == 0 {
		cfg.Port = 5060
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "sipvoice"
	}
	srv, err := net.ResolveUDPAddr("udp", net.JoinHostPort(cfg.Proxy, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.LocalPort})
	if err != nil {
		return nil, err
	}
	// figure out the local IP used towards the server
	probe, err := net.DialUDP("udp", nil, srv)
	if err != nil {
		return nil, err
	}
	lip := probe.LocalAddr().(*net.UDPAddr).IP.String()
	_ = probe.Close()
	s := &Client{cfg: cfg, conn: c, server: srv, localIP: lip, localPrt: c.LocalAddr().(*net.UDPAddr).Port,
		waiters: map[string]chan *Msg{}, onReq: map[string]func(*Msg){}, log: logf, traces: map[string]func(string, ...any){}}
	s.pubIP, s.pubPort = lip, s.localPrt
	go s.readLoop()
	return s, nil
}

func (s *Client) Close() { _ = s.conn.Close() }

func (s *Client) readLoop() {
	buf := make([]byte, 65535)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 4 || strings.TrimSpace(string(buf[:n])) == "" {
			continue // keepalive
		}
		m, err := parseSIP(append([]byte(nil), buf[:n]...))
		if err != nil {
			continue
		}
		cid := m.Get("Call-ID")
		s.trace(cid, "<<< from %s\n%s", from, redact(buf[:n]))
		if m.Status > 0 {
			s.learnNAT(m)
			s.mu.Lock()
			ch := s.waiters[cid]
			s.mu.Unlock()
			if ch == nil && m.Status >= 200 && m.Status < 300 && strings.HasSuffix(m.Get("CSeq"), "INVITE") {
				s.killLate(m) // 2xx for an abandoned INVITE: ACK + BYE so the far end never sits in silence
				continue
			}
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
			continue
		}
		s.mu.Lock()
		h := s.onReq[cid]
		s.mu.Unlock()
		if h != nil {
			h(m)
			continue
		}
		// Unsolicited request: never answer calls. 603 Decline for INVITE.
		code, reason := 200, "OK"
		if m.Method == "INVITE" {
			code, reason = 603, "Decline"
			s.log("rejected unsolicited inbound INVITE from %s", from)
		} else if m.Method == "ACK" {
			continue
		}
		s.Respond(m, code, reason, "", "")
	}
}

func (s *Client) learnNAT(m *Msg) {
	via := m.Get("Via")
	if r := regexp.MustCompile(`received=([0-9.]+)`).FindStringSubmatch(via); r != nil {
		s.pubIP = r[1]
	}
	if r := regexp.MustCompile(`rport=(\d+)`).FindStringSubmatch(via); r != nil {
		s.pubPort, _ = strconv.Atoi(r[1])
	}
}

func (s *Client) trace(cid, f string, a ...any) {
	s.mu.Lock()
	t := s.traces[cid]
	s.mu.Unlock()
	if t != nil {
		t(f, a...)
	}
}

// redact returns only the header block, with credentials removed.
func redact(b []byte) string {
	head := strings.SplitN(string(b), "\r\n\r\n", 2)[0]
	var out []string
	for _, l := range strings.Split(head, "\r\n") {
		ll := strings.ToLower(l)
		if strings.HasPrefix(ll, "authorization:") || strings.HasPrefix(ll, "proxy-authorization:") {
			l = l[:strings.Index(l, ":")+1] + " <redacted>"
		}
		out = append(out, "    "+l)
	}
	return strings.Join(out, "\n")
}

func cseqNum(m *Msg) int {
	n, _ := strconv.Atoi(strings.Fields(m.Get("CSeq") + " 0")[0])
	return n
}

// killLate ACKs and BYEs a 2xx that arrived for a dialog we no longer track.
func (s *Client) killLate(m *Msg) {
	s.log("late 2xx for abandoned call %s: sending ACK+BYE", m.Get("Call-ID"))
	target := m.Get("Contact")
	if r := regexp.MustCompile(`<([^>]+)>`).FindStringSubmatch(target); r != nil {
		target = r[1]
	}
	to := m.Get("To")
	uri := to
	if r := regexp.MustCompile(`<([^>]+)>`).FindStringSubmatch(to); r != nil {
		uri = r[1]
	}
	d := &Dialog{CallID: m.Get("Call-ID"), FromTag: toTag(m.Get("From")), ToTag: toTag(to), ToURI: uri, RemoteTarget: target}
	rr := m.All("Record-Route")
	for i := len(rr) - 1; i >= 0; i-- {
		for _, x := range strings.Split(rr[i], ",") {
			d.Route = append(d.Route, strings.TrimSpace(x))
		}
	}
	n := cseqNum(m)
	raw, _ := s.build("ACK", target, d, n, "", "", "")
	s.send(raw)
	raw, _ = s.build("BYE", target, d, n+1, "", "", "")
	s.send(raw)
}

func (s *Client) send(raw string) error {
	if i := strings.Index(raw, "Call-ID: "); i >= 0 {
		cid := strings.TrimSpace(strings.SplitN(raw[i+9:], "\r\n", 2)[0])
		s.trace(cid, ">>> to %s\n%s", s.server, redact([]byte(raw)))
	}
	_, err := s.conn.WriteToUDP([]byte(raw), s.server)
	return err
}

func (s *Client) Respond(req *Msg, code int, reason, extra, body string) {
	var b strings.Builder
	fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", code, reason)
	for _, v := range req.All("Via") {
		fmt.Fprintf(&b, "Via: %s\r\n", v)
	}
	to := req.Get("To")
	if code >= 300 && !strings.Contains(to, "tag=") {
		to += ";tag=" + randHex(4)
	}
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s",
		req.Get("From"), to, req.Get("Call-ID"), req.Get("CSeq"), extra, len(body), body)
	s.send(b.String())
}

func (s *Client) contact() string {
	return fmt.Sprintf("<sip:%s@%s:%d>", s.cfg.User, s.pubIP, s.pubPort)
}

// Dialog holds state for one outbound call.
type Dialog struct {
	CallID, FromTag, ToTag, RemoteTarget, ToURI string
	CSeq                                        int
	Route                                       []string
	inviteVia                                   map[int]string // exact Via of each INVITE sent, by CSeq
	ch                                          chan *Msg
}

func (s *Client) build(method, ruri string, d *Dialog, cseq int, extra, body, ctype string) (string, string) {
	branch := "z9hG4bK" + randHex(8)
	to := "<" + d.ToURI + ">"
	if d.ToTag != "" {
		to += ";tag=" + d.ToTag
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", method, ruri)
	fmt.Fprintf(&b, "Via: SIP/2.0/UDP %s:%d;branch=%s;rport\r\n", s.pubIP, s.pubPort, branch)
	fmt.Fprintf(&b, "Max-Forwards: 70\r\n")
	for _, r := range d.Route {
		fmt.Fprintf(&b, "Route: %s\r\n", r)
	}
	fmt.Fprintf(&b, "From: <sip:%s@%s>;tag=%s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %d %s\r\n",
		s.cfg.User, s.cfg.Domain, d.FromTag, to, d.CallID, cseq, method)
	fmt.Fprintf(&b, "Contact: %s\r\nUser-Agent: %s\r\nAllow: INVITE, ACK, BYE, CANCEL, OPTIONS\r\n%s", s.contact(), s.cfg.UserAgent, extra)
	if ctype != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\n", ctype)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return b.String(), branch
}

func (s *Client) NewDialog(toURI string) *Dialog {
	d := &Dialog{CallID: randHex(12) + "@sipvoice", FromTag: randHex(5), ToURI: toURI, CSeq: 1, ch: make(chan *Msg, 32)}
	s.mu.Lock()
	s.waiters[d.CallID] = d.ch
	s.mu.Unlock()
	return d
}
func (s *Client) SetTrace(d *Dialog, f func(string, ...any)) {
	s.mu.Lock()
	s.traces[d.CallID] = f
	s.mu.Unlock()
}

func (s *Client) DropDialog(d *Dialog) {
	s.mu.Lock()
	delete(s.traces, d.CallID)
	delete(s.waiters, d.CallID)
	delete(s.onReq, d.CallID)
	s.mu.Unlock()
}

// request sends a non-INVITE request and handles digest auth, returning the final response.
func (s *Client) request(method, ruri string, d *Dialog, extra string) (*Msg, error) {
	auth := ""
	for attempt := 0; attempt < 3; attempt++ {
		raw, _ := s.build(method, ruri, d, d.CSeq, extra+auth, "", "")
		var resp *Msg
		for retry := 0; retry < 7 && resp == nil; retry++ {
			s.send(raw)
			t := time.After(time.Duration(500<<min(retry, 3)) * time.Millisecond)
		wait:
			for {
				select {
				case m := <-d.ch:
					if m.Status >= 200 && strings.HasSuffix(m.Get("CSeq"), method) && cseqNum(m) == d.CSeq {
						resp = m
						break wait
					}
				case <-t:
					break wait
				}
			}
		}
		if resp == nil {
			return nil, fmt.Errorf("%s: no response from %s (UDP blocked?)", method, s.server)
		}
		d.CSeq++
		if (resp.Status == 401 || resp.Status == 407) && attempt == 0 {
			auth = s.authHeader(resp, method, ruri)
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("auth failed")
}

func (s *Client) authHeader(resp *Msg, method, uri string) string {
	if resp.Status == 401 {
		return "Authorization: " + digest(resp.Get("WWW-Authenticate"), method, uri, s.cfg.AuthUser, s.cfg.Password, 1) + "\r\n"
	}
	return "Proxy-Authorization: " + digest(resp.Get("Proxy-Authenticate"), method, uri, s.cfg.AuthUser, s.cfg.Password, 1) + "\r\n"
}

// Probe sends an authenticated OPTIONS to the domain. No call, no registration.
func (s *Client) Probe() (string, error) {
	uri := "sip:" + s.cfg.Domain
	d := s.NewDialog(uri)
	defer s.DropDialog(d)
	r, err := s.request("OPTIONS", uri, d, "")
	if err != nil {
		return "", err
	}
	return r.StartLine, nil
}

func (s *Client) ack(d *Dialog, cseq int, resp *Msg, auth string) {
	ruri := d.RemoteTarget
	if resp.Status >= 300 {
		ruri = d.ToURI
	}
	saveTag := d.ToTag
	d.ToTag = toTag(resp.Get("To"))
	raw, _ := s.build("ACK", ruri, d, cseq, auth, "", "")
	if resp.Status >= 300 {
		d.ToTag = saveTag
		// ACK for non-2xx is part of the INVITE transaction: identical Via
		// (sent-by + branch) and Request-URI as the original INVITE.
		if v, ok := d.inviteVia[cseq]; ok {
			raw = strings.Replace(raw, viaLine(raw), v, 1)
		}
	}
	s.send(raw)
}

func viaLine(raw string) string {
	for _, l := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(l, "Via: ") {
			return l
		}
	}
	return ""
}

func toTag(to string) string {
	if r := regexp.MustCompile(`tag=([^;>\s]+)`).FindStringSubmatch(to); r != nil {
		return r[1]
	}
	return ""
}

// Invite places an outbound call. ringing is called on 18x. Returns the answer SDP.
func (s *Client) Invite(d *Dialog, sdp string, timeout time.Duration, cancel <-chan struct{}, ringing func()) (*Msg, error) {
	auth := ""
	deadline := time.After(timeout)
	for attempt := 0; attempt < 2; attempt++ {
		cseq := d.CSeq
		raw, branch := s.build("INVITE", d.ToURI, d, cseq, auth, sdp, "application/sdp")
		if d.inviteVia == nil {
			d.inviteVia = map[int]string{}
		}
		d.inviteVia[cseq] = viaLine(raw)
		s.send(raw)
		retrans := time.NewTicker(500 * time.Millisecond)
		provisional := false
		for {
			select {
			case <-cancel:
				retrans.Stop()
				s.cancelInvite(d, cseq, branch)
				return nil, fmt.Errorf("cancelled")
			case <-deadline:
				retrans.Stop()
				s.cancelInvite(d, cseq, branch)
				return nil, fmt.Errorf("no answer (timeout)")
			case <-retrans.C:
				if !provisional {
					s.send(raw)
				}
			case m := <-d.ch:
				if !strings.HasSuffix(m.Get("CSeq"), "INVITE") {
					continue
				}
				if cseqNum(m) != cseq {
					// stale response from an earlier transaction (e.g. retransmitted 401)
					if m.Status >= 300 {
						s.ack(d, cseqNum(m), m, "")
					}
					continue
				}
				if m.Status < 200 {
					provisional = true
					if m.Status >= 180 && ringing != nil {
						ringing()
					}
					continue
				}
				retrans.Stop()
				d.CSeq++
				if m.Status >= 300 {
					s.ack(d, cseq, m, "")
					if (m.Status == 401 || m.Status == 407) && attempt == 0 {
						auth = s.authHeader(m, "INVITE", d.ToURI)
						goto next
					}
					return nil, fmt.Errorf("call failed: %s", m.StartLine)
				}
				d.ToTag = toTag(m.Get("To"))
				if c := m.Get("Contact"); c != "" {
					if r := regexp.MustCompile(`<([^>]+)>`).FindStringSubmatch(c); r != nil {
						d.RemoteTarget = r[1]
					} else {
						d.RemoteTarget = strings.Split(c, ";")[0]
					}
				} else {
					d.RemoteTarget = d.ToURI
				}
				rr := m.All("Record-Route")
				d.Route = nil
				for i := len(rr) - 1; i >= 0; i-- {
					for _, x := range strings.Split(rr[i], ",") {
						d.Route = append(d.Route, strings.TrimSpace(x))
					}
				}
				s.ack(d, cseq, m, "")
				return m, nil
			}
		}
	next:
	}
	return nil, fmt.Errorf("auth failed")
}

func (s *Client) cancelInvite(d *Dialog, cseq int, branch string) {
	var b strings.Builder
	fmt.Fprintf(&b, "CANCEL %s SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=%s;rport\r\nMax-Forwards: 70\r\n", d.ToURI, s.pubIP, s.pubPort, branch)
	fmt.Fprintf(&b, "From: <sip:%s@%s>;tag=%s\r\nTo: <%s>\r\nCall-ID: %s\r\nCSeq: %d CANCEL\r\nContent-Length: 0\r\n\r\n", s.cfg.User, s.cfg.Domain, d.FromTag, d.ToURI, d.CallID, cseq)
	s.send(b.String())
}

func (s *Client) Bye(d *Dialog) {
	r, err := s.request("BYE", d.RemoteTarget, d, "")
	if err != nil {
		s.log("BYE: %v", err)
	} else {
		s.log("BYE: %s", r.StartLine)
	}
}

// OnDialogRequest installs a handler for in-dialog requests (remote BYE).
func (s *Client) OnDialogRequest(d *Dialog, h func(*Msg)) {
	s.mu.Lock()
	s.onReq[d.CallID] = h
	s.mu.Unlock()
}

// PublicAddr is the address the server saw us at (learned from Via received/rport).
func (s *Client) PublicAddr() (string, int) { return s.pubIP, s.pubPort }
