package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExpandAndDefaults(t *testing.T) {
	t.Setenv("TEST_SIP_PW", "s3cret")
	t.Setenv("XAI_API_KEY", "from-env")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("sip:\n  domain: sip.example.net\n  username: alice\n  password: ${TEST_SIP_PW}\nallowlist: ['+15550100']\n"), 0o600)
	c, err := Load(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.SIP.Password != "s3cret" || c.XAI.APIKey != "from-env" || c.SIP.AuthUsername != "alice" || c.SIP.Proxy != "sip.example.net" || c.SIP.Port != 5060 {
		t.Fatalf("bad config: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingExplicitFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), true); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateReportsMissing(t *testing.T) {
	t.Setenv("SIP_USERNAME", "")
	t.Setenv("SIP_PASSWORD", "")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("SIP_DOMAIN", "")
	c, _ := Load(filepath.Join(t.TempDir(), "none.yaml"), false)
	if c.Validate() == nil {
		t.Fatal("expected validation error")
	}
}
