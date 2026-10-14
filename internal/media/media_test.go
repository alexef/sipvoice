package media

import "testing"

func TestParseAnswer(t *testing.T) {
	a, pt, err := ParseAnswer("v=0\r\nc=IN IP4 198.51.100.4\r\nm=audio 30000 RTP/AVP 0 101\r\n")
	if err != nil || pt != 0 || a.Port != 30000 || a.IP.String() != "198.51.100.4" {
		t.Fatal(a, pt, err)
	}
	if _, _, err := ParseAnswer("v=0\r\nc=IN IP4 198.51.100.4\r\nm=audio 30000 RTP/AVP 9\r\n"); err == nil {
		t.Fatal("expected no-G.711 error")
	}
}

func TestDTMFCodes(t *testing.T) {
	for _, c := range "0123456789*#ABCD" {
		e, ok := dtmfCode(c)
		if !ok || dtmfName(e) != string(c) {
			t.Errorf("roundtrip %c", c)
		}
	}
}
