// Package media implements a G.711 RTP session with RFC 4733 DTMF.
package media

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RTP session carrying G.711 (PCMU=0 / PCMA=8) and RFC 4733 DTMF (pt 101).
type RTP struct {
	conn       *net.UDPConn
	remote     *net.UDPAddr
	pt         byte
	ssrc       uint32
	seq        uint16
	ts         uint32
	mu         sync.Mutex
	out        []byte // pending outbound audio (G.711 bytes)
	OnInput    func([]byte)
	OnDTMF     func(string)
	closed     chan struct{}
	lastDTMFts uint32
}

func NewRTP(port int) (*RTP, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	return &RTP{conn: c, ssrc: rand.Uint32(), seq: uint16(rand.Intn(65535)), ts: rand.Uint32(), closed: make(chan struct{})}, nil
}
func (r *RTP) Port() int { return r.conn.LocalAddr().(*net.UDPAddr).Port }

func OfferSDP(ip string, port int) string {
	id := strconv.FormatInt(time.Now().Unix(), 10)
	return fmt.Sprintf("v=0\r\no=- %s %s IN IP4 %s\r\ns=sipvoice\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8 0 101\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-15\r\na=ptime:20\r\na=sendrecv\r\n", id, id, ip, ip, port)
}

// ParseAnswer returns remote addr and chosen payload type.
func ParseAnswer(sdp string) (*net.UDPAddr, byte, error) {
	ip := ""
	if m := regexp.MustCompile(`c=IN IP4 ([0-9.]+)`).FindStringSubmatch(sdp); m != nil {
		ip = m[1]
	}
	m := regexp.MustCompile(`m=audio (\d+) RTP/AVP ([0-9 ]+)`).FindStringSubmatch(sdp)
	if m == nil || ip == "" {
		return nil, 0, fmt.Errorf("bad SDP answer")
	}
	port, _ := strconv.Atoi(m[1])
	var pt byte = 255
	for _, f := range strings.Fields(m[2]) {
		if f == "0" || f == "8" {
			v, _ := strconv.Atoi(f)
			pt = byte(v)
			break
		}
	}
	if pt == 255 {
		return nil, 0, fmt.Errorf("remote offers no G.711")
	}
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: port}, pt, nil
}

func (r *RTP) Codec() string {
	if r.pt == 8 {
		return "audio/pcma"
	}
	return "audio/pcmu"
}
func (r *RTP) silence() byte {
	if r.pt == 8 {
		return 0xD5
	}
	return 0xFF
}

func (r *RTP) Start(remote *net.UDPAddr, pt byte) {
	r.remote, r.pt = remote, pt
	go r.recvLoop()
	go r.sendLoop()
}

func (r *RTP) Close() {
	select {
	case <-r.closed:
	default:
		close(r.closed)
		_ = r.conn.Close()
	}
}

func (r *RTP) recvLoop() {
	buf := make([]byte, 2048)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		// symmetric RTP: latch to where media actually comes from
		r.mu.Lock()
		r.remote = from
		r.mu.Unlock()
		cc := int(buf[0] & 0x0f)
		hl := 12 + 4*cc
		if buf[0]&0x10 != 0 && n > hl+4 {
			hl += 4 + 4*int(binary.BigEndian.Uint16(buf[hl+2:]))
		}
		if hl > n {
			continue
		}
		pt := buf[1] & 0x7f
		ts := binary.BigEndian.Uint32(buf[4:])
		payload := buf[hl:n]
		if pt == 101 && len(payload) >= 4 {
			if payload[1]&0x80 != 0 && ts != r.lastDTMFts && r.OnDTMF != nil {
				r.lastDTMFts = ts
				r.OnDTMF(dtmfName(payload[0]))
			}
			continue
		}
		if pt == r.pt && r.OnInput != nil {
			r.OnInput(append([]byte(nil), payload...))
		}
	}
}

func (r *RTP) packet(pt byte, marker bool, ts uint32, payload []byte) []byte {
	p := make([]byte, 12+len(payload))
	p[0] = 0x80
	p[1] = pt
	if marker {
		p[1] |= 0x80
	}
	binary.BigEndian.PutUint16(p[2:], r.seq)
	r.seq++
	binary.BigEndian.PutUint32(p[4:], ts)
	binary.BigEndian.PutUint32(p[8:], r.ssrc)
	copy(p[12:], payload)
	return p
}

func (r *RTP) write(p []byte) {
	r.mu.Lock()
	a := r.remote
	r.mu.Unlock()
	r.conn.WriteToUDP(p, a)
}

func (r *RTP) sendLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	frame := make([]byte, 160)
	for {
		select {
		case <-r.closed:
			return
		case <-t.C:
		}
		r.mu.Lock()
		n := copy(frame, r.out)
		r.out = r.out[n:]
		r.mu.Unlock()
		for i := n; i < 160; i++ {
			frame[i] = r.silence()
		}
		r.write(r.packet(r.pt, false, r.ts, frame))
		r.ts += 160
	}
}

func (r *RTP) Enqueue(b []byte) { r.mu.Lock(); r.out = append(r.out, b...); r.mu.Unlock() }
func (r *RTP) Flush()           { r.mu.Lock(); r.out = nil; r.mu.Unlock() }
func (r *RTP) Pending() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Duration(len(r.out)) * time.Second / 8000
}

// SendDTMF sends RFC 4733 events for each digit.
func (r *RTP) SendDTMF(digits string) {
	for _, d := range digits {
		ev, ok := dtmfCode(d)
		if !ok {
			continue
		}
		r.mu.Lock()
		ts := r.ts
		r.mu.Unlock()
		for i := 1; i <= 8; i++ {
			dur := uint16(i * 160)
			end := i >= 6
			pl := []byte{ev, 10, byte(dur >> 8), byte(dur)}
			if end {
				pl[1] |= 0x80
			}
			r.write(r.packet(101, i == 1, ts, pl))
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(80 * time.Millisecond)
	}
}

func dtmfCode(c rune) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return byte(c - '0'), true
	case c == '*':
		return 10, true
	case c == '#':
		return 11, true
	case c >= 'A' && c <= 'D':
		return byte(12 + c - 'A'), true
	}
	return 0, false
}
func dtmfName(e byte) string {
	if e <= 9 {
		return string(rune('0' + e))
	}
	return map[byte]string{10: "*", 11: "#", 12: "A", 13: "B", 14: "C", 15: "D"}[e]
}
