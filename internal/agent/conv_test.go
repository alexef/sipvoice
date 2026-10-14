package agent

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func TestGreetFallback(t *testing.T) {
	s := NewState(t0)
	if s.Tick(at(2000), 0).Greet {
		t.Fatal("greeted too early")
	}
	if !s.Tick(at(2600), 0).Greet {
		t.Fatal("no fallback greet")
	}
	if s.Tick(at(3000), 0).Greet {
		t.Fatal("greeted twice")
	}
}

func TestGreetWaitsForHello(t *testing.T) {
	s := NewState(t0)
	s.OnSpeechStarted()
	if s.Tick(at(3000), 0).Greet {
		t.Fatal("must not talk over callee")
	}
	s.OnSpeechStopped()
	if s.Tick(at(3100), 0).Greet {
		t.Fatal("server VAD replies; no explicit greet")
	}
}

func TestEndCallAfterGoodbye(t *testing.T) {
	s := NewState(t0)
	s.greeted = true
	s.OnResponseCreated()
	s.OnAssistantText("Thank you, goodbye!", at(100))
	s.OnEndCall("done", at(200))
	if s.OnEndCall("again", at(300)) == "" || !strings.Contains(s.OnEndCall("x", at(300)), "already") {
		t.Fatal("second end_call not ignored")
	}
	if s.Tick(at(400), time.Second).Hangup != "" {
		t.Fatal("hung up during playout")
	}
	s.OnResponseDone(at(500))
	if s.Tick(at(600), 0).Hangup == "" {
		t.Fatal("no hangup after drain")
	}
}

func TestEndCallCap(t *testing.T) {
	s := NewState(t0)
	s.greeted = true
	s.OnAssistantText("Goodbye", at(0))
	s.OnResponseCreated()
	s.OnEndCall("x", at(0))
	if s.Tick(at(3100), time.Second).Hangup == "" {
		t.Fatal("3s cap not applied")
	}
}

func TestEndCallWithoutGoodbyeInjects(t *testing.T) {
	s := NewState(t0)
	s.greeted = true
	s.OnResponseCreated()
	s.OnAssistantText("Yes, that is correct.", at(0))
	s.OnEndCall("x", at(100))
	if a := s.Tick(at(200), 0); a.SayGoodbye || a.Hangup != "" {
		t.Fatal("must wait for active response")
	}
	s.OnResponseDone(at(300))
	if !s.Tick(at(400), 0).SayGoodbye {
		t.Fatal("goodbye not requested")
	}
	s.OnResponseCreated()
	if s.Tick(at(1000), 0).Hangup != "" {
		t.Fatal("hung up before goodbye")
	}
	s.OnResponseDone(at(1500))
	if s.Tick(at(1600), time.Second).Hangup != "" {
		t.Fatal("hung up during goodbye playout")
	}
	if s.Tick(at(1700), 0).Hangup == "" {
		t.Fatal("no hangup after goodbye drained")
	}
}

func TestOffTaskThreeStrikes(t *testing.T) {
	s := NewState(t0)
	s.greeted = true
	for i := 0; i < 2; i++ {
		s.OnResponseCreated()
		s.OnOffTask(at(i * 1000))
		s.OnAudio(at(i*1000 + 10))
		s.OnResponseDone(at(i*1000 + 500))
	}
	if s.offTaskCount != 2 {
		t.Fatal("count", s.offTaskCount)
	}
	// on-task turn resets
	s.OnResponseCreated()
	s.OnResponseDone(at(2500))
	if s.offTaskCount != 0 {
		t.Fatal("not reset")
	}
	for i := 0; i < 3; i++ {
		s.OnResponseCreated()
		s.OnOffTask(at(3000 + i*1000))
		s.OnResponseDone(at(3000 + i*1000 + 500))
	}
	if !s.endPending || !s.goodbyeNeeded {
		t.Fatal("3 strikes did not start goodbye")
	}
	if !s.Tick(at(6000), 0).SayGoodbye {
		t.Fatal("goodbye not requested")
	}
}

func TestOffTaskToolOnlyContinues(t *testing.T) {
	s := NewState(t0)
	s.greeted = true
	s.OnResponseCreated()
	s.OnOffTask(at(0))
	s.OnResponseDone(at(100))
	if !s.Tick(at(200), 0).Continue {
		t.Fatal("refusal not spoken")
	}
}

func TestAutoByeAfterGoodbye(t *testing.T) {
	s := NewState(t0)
	s.greeted = true
	s.OnResponseCreated()
	s.OnAudio(at(0))
	s.OnAssistantText("Bye!", at(100))
	s.OnResponseDone(at(200))
	if s.Tick(at(3000), 0).Hangup != "" {
		t.Fatal("too early")
	}
	if s.Tick(at(4300), 0).Hangup == "" {
		t.Fatal("no auto bye")
	}
}
