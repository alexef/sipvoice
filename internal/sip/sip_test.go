package sip

import (
	"strings"
	"testing"
)

func TestParseResponse(t *testing.T) {
	m, err := parseSIP([]byte("SIP/2.0 401 Unauthorized\r\nv: SIP/2.0/UDP 10.0.0.2:5060;branch=z9hG4bKx;received=203.0.113.7;rport=4000\r\nCSeq: 7 INVITE\r\nTo: <sip:bob@example.net>;tag=abc\r\nContent-Length: 0\r\n\r\n"))
	if err != nil || m.Status != 401 || cseqNum(m) != 7 || toTag(m.Get("To")) != "abc" || m.Get("Via") == "" {
		t.Fatalf("bad parse: %+v", m)
	}
}

func TestDigestRFC2617(t *testing.T) {
	// RFC 2617 section 3.5 example values
	ch := `Digest realm="testrealm@host.com", qop="auth", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`
	h := digest(ch, "GET", "/dir/index.html", "Mufasa", "Circle Of Life", 1)
	for _, want := range []string{`username="Mufasa"`, `qop=auth`, `nc=00000001`, `opaque="5ccc069c403ebaf9f0171e9517f40e41"`} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
	noQop := digest(`Digest realm="r", nonce="n"`, "INVITE", "sip:x@y", "u", "p", 1)
	if !strings.Contains(noQop, `response="`+md5h(md5h("u:r:p")+":n:"+md5h("INVITE:sip:x@y"))+`"`) {
		t.Error("bad legacy digest", noQop)
	}
}

func TestRedact(t *testing.T) {
	r := redact([]byte("INVITE sip:x SIP/2.0\r\nAuthorization: Digest secret\r\nProxy-Authorization: Digest secret\r\n\r\nv=0 body"))
	if strings.Contains(r, "secret") || strings.Contains(r, "body") {
		t.Fatal(r)
	}
}
