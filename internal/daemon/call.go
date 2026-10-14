package daemon

import (
	"github.com/alexef/sipvoice/internal/agent"
	"github.com/alexef/sipvoice/internal/config"
	"github.com/alexef/sipvoice/internal/media"
	"github.com/alexef/sipvoice/internal/sip"

	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type CallRequest struct {
	To           string `json:"to"`
	Instructions string `json:"instructions"`
	Lang         string `json:"lang"`
	MaxSeconds   int    `json:"max_seconds,omitempty"`
}

type Turn struct {
	T    string `json:"t"`
	Role string `json:"role"`
	Text string `json:"text"`
}

type Call struct {
	ID         string          `json:"id"`
	Req        CallRequest     `json:"request"`
	State      string          `json:"state"` // dialing, ringing, active, ended, failed
	Reason     string          `json:"end_reason,omitempty"`
	Started    time.Time       `json:"started"`
	Answered   *time.Time      `json:"answered,omitempty"`
	Ended      *time.Time      `json:"ended,omitempty"`
	Transcript []Turn          `json:"transcript"`
	Summary    string          `json:"summary,omitempty"`
	Outcome    json.RawMessage `json:"outcome,omitempty"`
	DTMFRecv   string          `json:"dtmf_received,omitempty"`

	mu      sync.Mutex
	hangup  chan struct{}
	once    sync.Once
	dir     string
	logf    *os.File
	dlg     *sip.Dialog
	byeOnce sync.Once
}

// sendBye ends an answered dialog exactly once (safe to call from any path).
func (d *Daemon) sendBye(c *Call) {
	c.mu.Lock()
	dlg := c.dlg
	c.mu.Unlock()
	if dlg == nil {
		return
	}
	c.byeOnce.Do(func() { d.sip.Bye(dlg) })
}

func (c *Call) Hangup(reason string) {
	c.once.Do(func() {
		c.mu.Lock()
		if c.Reason == "" {
			c.Reason = reason
		}
		c.mu.Unlock()
		close(c.hangup)
	})
}

func (c *Call) setState(s string) { c.mu.Lock(); c.State = s; c.mu.Unlock(); c.log("state=%s", s) }
func (c *Call) log(f string, a ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(f, a...))
	if c.logf != nil {
		c.logf.WriteString(line)
	}
	fmt.Fprintf(os.Stderr, "[%s] %s", c.ID, line)
}
func (c *Call) addTurn(role, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	c.mu.Lock()
	c.Transcript = append(c.Transcript, Turn{time.Now().Format(time.RFC3339), role, text})
	c.mu.Unlock()
	c.log("%s: %s", role, text)
}

func (d *Daemon) runCall(c *Call) {
	defer d.finish(c)
	defer func() {
		if r := recover(); r != nil {
			c.log("panic: %v", r)
			c.Reason = fmt.Sprint("panic: ", r)
			d.sendBye(c)
		}
	}()
	cfg := d.cfg
	toURI := fmt.Sprintf("sip:%s@%s", DialString(c.Req.To, cfg.SIP.PlusReplacement), cfg.SIP.Domain)
	rtp, err := media.NewRTP(0)
	if err != nil {
		c.State, c.Reason = "failed", err.Error()
		return
	}
	defer rtp.Close()
	dlg := d.sip.NewDialog(toURI)
	defer d.sip.DropDialog(dlg)
	d.sip.SetTrace(dlg, c.log)
	remoteBye := make(chan struct{})
	var byeOnce sync.Once
	d.sip.OnDialogRequest(dlg, func(m *sip.Msg) {
		switch m.Method {
		case "BYE":
			d.sip.Respond(m, 200, "OK", "", "")
			byeOnce.Do(func() { close(remoteBye) })
		case "ACK":
		default:
			d.sip.Respond(m, 200, "OK", "", "")
		}
	})
	pubIP, _ := d.sip.PublicAddr()
	sdp := media.OfferSDP(pubIP, rtp.Port())
	c.setState("dialing")
	ans, err := d.sip.Invite(dlg, sdp, time.Duration(cfg.Limits.RingSeconds)*time.Second, c.hangup, func() {
		if c.State != "ringing" {
			c.setState("ringing")
		}
	})
	if err != nil {
		c.setState("failed")
		c.Reason = err.Error()
		return
	}
	now := time.Now()
	c.Answered = &now
	c.mu.Lock()
	c.dlg = dlg
	c.mu.Unlock()
	defer d.sendBye(c)
	raddr, pt, err := media.ParseAnswer(ans.Body)
	if err != nil {
		c.Reason = err.Error()
		d.sendBye(c)
		c.setState("failed")
		return
	}
	c.setState("active")

	// xAI realtime
	h := http.Header{"Authorization": {"Bearer " + cfg.XAI.APIKey}}
	ws, resp, err := websocket.DefaultDialer.Dial(cfg.XAI.RealtimeURL+"?model="+cfg.XAI.VoiceModel, h)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		c.Reason = "xai connect: " + err.Error()
		d.sendBye(c)
		c.setState("failed")
		return
	}
	defer ws.Close()
	var wmu sync.Mutex
	send := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		ws.WriteJSON(v)
	}
	rtpCodec := map[byte]string{0: "audio/pcmu", 8: "audio/pcma"}[pt]
	send(map[string]any{"type": "session.update", "session": map[string]any{
		"voice": cfg.XAI.Voice, "instructions": agent.SystemPrompt(cfg.OwnerName, c.Req.Lang, c.Req.Instructions),
		"turn_detection": map[string]any{"type": "server_vad"},
		"tools":          agent.Tools,
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": rtpCodec}, "transcription": map[string]any{"language_hint": c.Req.Lang}},
			"output": map[string]any{"format": map[string]any{"type": rtpCodec}},
		}}})
	rtp.OnInput = func(b []byte) {
		send(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(b)})
	}
	rtp.OnDTMF = func(s string) { c.mu.Lock(); c.DTMFRecv += s; c.mu.Unlock(); c.log("dtmf in: %s", s) }
	rtp.Start(raddr, pt)
	// Do not speak first: wait for the callee's hello (or 2.5s of silence).
	cs := agent.NewState(time.Now())
	goodbyeLine := map[string]string{"en": "Goodbye!", "de": "Auf Wiederhören!", "fr": "Au revoir !", "it": "Arrivederci!", "es": "¡Adiós!", "ro": "La revedere!"}[c.Req.Lang]
	if goodbyeLine == "" {
		goodbyeLine = "Goodbye!"
	}
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				c.Hangup("ai disconnected")
				return
			}
			var ev map[string]any
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			typ, _ := ev["type"].(string)
			switch typ {
			case "response.created":
				cs.OnResponseCreated()
			case "response.done":
				cs.OnResponseDone(time.Now())
			case "response.output_audio.delta", "response.audio.delta":
				if s, ok := ev["delta"].(string); ok {
					b, _ := base64.StdEncoding.DecodeString(s)
					rtp.Enqueue(b)
					cs.OnAudio(time.Now())
				}
			case "input_audio_buffer.speech_started":
				cs.OnSpeechStarted()
				if cs.BargeInAllowed() {
					rtp.Flush() // barge-in (never cuts the final goodbye)
				}
			case "input_audio_buffer.speech_stopped":
				cs.OnSpeechStopped()
			case "response.output_audio_transcript.done", "response.audio_transcript.done":
				s, _ := ev["transcript"].(string)
				c.addTurn("assistant", s)
				cs.OnAssistantText(s, time.Now())
			case "conversation.item.input_audio_transcription.completed":
				s, _ := ev["transcript"].(string)
				c.addTurn("callee", s)
			case "response.function_call_arguments.done":
				name, _ := ev["name"].(string)
				callID, _ := ev["call_id"].(string)
				var args map[string]any
				if err := json.Unmarshal([]byte(fmt.Sprint(ev["arguments"])), &args); err != nil {
					c.log("bad tool arguments: %v", err)
				}
				c.log("tool %s %v", name, args)
				out := `{"ok":true}`
				switch name {
				case "end_call":
					out = cs.OnEndCall(fmt.Sprint(args["result"]), time.Now())
				case "report_off_task":
					out = cs.OnOffTask(time.Now())
					c.log("off-task: %v", args["summary"])
				case "send_dtmf":
					go rtp.SendDTMF(fmt.Sprint(args["digits"]))
				default:
					out = `{"error":"unknown tool"}`
				}
				send(map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "function_call_output", "call_id": callID, "output": out}})
				if name == "send_dtmf" {
					send(map[string]any{"type": "response.create"})
				}
			case "error":
				c.log("xai error: %s", string(data))
			}
		}
	}()

	maxDur := time.Duration(c.Req.MaxSeconds) * time.Second
	limit := time.NewTimer(maxDur)
	defer limit.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.hangup:
			d.sendBye(c)
			return
		case <-remoteBye:
			c.Hangup("remote hung up")
			c.byeOnce.Do(func() {}) // dialog already terminated by the far end
			return
		case <-limit.C:
			c.Hangup(fmt.Sprintf("max duration %s reached", maxDur))
			d.sendBye(c)
			return
		case <-tick.C:
		}
		act := cs.Tick(time.Now(), rtp.Pending())
		switch {
		case act.Greet:
			c.log("no hello from callee after %s; greeting first", agent.GreetFallback)
			send(map[string]any{"type": "response.create"})
		case act.Continue:
			send(map[string]any{"type": "response.create"})
		case act.SayGoodbye:
			c.log("injecting goodbye before hangup")
			send(map[string]any{"type": "response.create", "response": map[string]any{
				"instructions": fmt.Sprintf("Say only a brief polite goodbye in %s, for example %q. Nothing else. Do not call any tool.", agent.LangName(c.Req.Lang), goodbyeLine)}})
		case act.Hangup != "":
			time.Sleep(300 * time.Millisecond) // far-end jitter buffer tail
			c.Hangup(act.Hangup)
			d.sendBye(c)
			return
		}
	}
}

// NormalizeNumber strips formatting characters from a phone number.
func NormalizeNumber(n string) string {
	return strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", "/", "", ".", "").Replace(n)
}

// DialString is the user part used in the INVITE Request-URI. If plus is set
// (e.g. "00"), a leading "+" is replaced with it.
func DialString(n, plus string) string {
	n = NormalizeNumber(n)
	if plus != "" && strings.HasPrefix(n, "+") {
		n = plus + n[1:]
	}
	return n
}

func (d *Daemon) finish(c *Call) {
	now := time.Now()
	c.mu.Lock()
	c.Ended = &now
	if c.State != "failed" {
		c.State = "ended"
	}
	c.mu.Unlock()
	c.log("ended: %s", c.Reason)
	d.writeResults(c)
	if len(c.Transcript) > 0 {
		sum, outcome, err := summarize(d.cfg, c)
		if err != nil {
			c.log("summary failed: %v", err)
		} else {
			c.Summary, c.Outcome = sum, outcome
		}
	}
	d.writeResults(c)
	if c.logf != nil {
		c.logf.Close()
	}
	d.mu.Lock()
	d.active = nil
	d.mu.Unlock()
}

func (d *Daemon) writeResults(c *Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var tr strings.Builder
	for _, t := range c.Transcript {
		fmt.Fprintf(&tr, "[%s] %s: %s\n", t.T, t.Role, t.Text)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "transcript.txt"), []byte(tr.String()), 0o600); err != nil {
		c.log("write transcript: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "summary.md"), []byte(c.Summary+"\n"), 0o600); err != nil {
		c.log("write summary: %v", err)
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(filepath.Join(c.dir, "result.json"), b, 0o600); err != nil {
		c.log("write result: %v", err)
	}
}

func summarize(cfg *config.Config, c *Call) (string, json.RawMessage, error) {
	var tr strings.Builder
	for _, t := range c.Transcript {
		fmt.Fprintf(&tr, "%s: %s\n", t.Role, t.Text)
	}
	prompt := fmt.Sprintf("Task given to the phone assistant:\n%s\n\nTranscript:\n%s\nReturn JSON only: {\"summary\": \"2-4 sentences in English\", \"task_completed\": bool, \"key_facts\": {..}}", c.Req.Instructions, tr.String())
	body, _ := json.Marshal(map[string]any{"model": cfg.XAI.SummaryModel, "messages": []map[string]string{{"role": "user", "content": prompt}},
		"response_format": map[string]string{"type": "json_object"}})
	req, _ := http.NewRequest("POST", "https://api.x.ai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.XAI.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", nil, err
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("empty")
	}
	content := out.Choices[0].Message.Content
	var parsed struct{ Summary string }
	_ = json.Unmarshal([]byte(content), &parsed)
	if !json.Valid([]byte(content)) {
		return content, nil, nil
	}
	return parsed.Summary, json.RawMessage(content), nil
}
