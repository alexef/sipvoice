package agent

import "testing"

func TestGoodbye(t *testing.T) {
	for _, s := range []string{"Thanks, goodbye!", "Bye.", "Danke, auf Wiederhören!", "Tschüss.", "Merci, au revoir", "Ciao!"} {
		if !IsGoodbye(s) {
			t.Error(s)
		}
	}
	for _, s := range []string{"What is the tracking number?", "Yes, that is correct.", "Goodbye is not what I meant, can you repeat?", "Byers street"} {
		if IsGoodbye(s) {
			t.Error("false positive:", s)
		}
	}
}
