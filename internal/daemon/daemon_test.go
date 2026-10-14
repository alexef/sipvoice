package daemon

import "testing"

func TestAllowed(t *testing.T) {
	l := []string{"+1 555 0100", "+44-20-7946-0000"}
	for _, n := range []string{"+15550100", "+44 20 7946 0000", "+1 (555) 0100"} {
		if !Allowed(l, n) {
			t.Error("should allow", n)
		}
	}
	for _, n := range []string{"", "+15550101", "15550100"} {
		if Allowed(l, n) {
			t.Error("should block", n)
		}
	}
	if Allowed(nil, "+15550100") {
		t.Error("empty allowlist must block")
	}
}

func TestDialString(t *testing.T) {
	if got := DialString("+1 555 0100", "00"); got != "0015550100" {
		t.Error(got)
	}
	if got := DialString("+1 555 0100", ""); got != "+15550100" {
		t.Error(got)
	}
}
