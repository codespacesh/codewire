package tunnel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/wgtunnel/tunnelsdk"
)

func TestLoadOrGenerateKey_GeneratesNew(t *testing.T) {
	dir := t.TempDir()

	key, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	if key.IsZero() {
		t.Fatal("generated key is zero")
	}
	if !key.IsPrivate() {
		t.Fatal("generated key is not private")
	}

	// File should exist with correct permissions.
	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if info.Mode().Perm() != keyFilePerms {
		t.Fatalf("expected perms %o, got %o", keyFilePerms, info.Mode().Perm())
	}
}

func TestLoadOrGenerateKey_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	key1, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	key2, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	if key1.String() != key2.String() {
		t.Fatalf("keys differ after reload: %s != %s", key1.String(), key2.String())
	}
}

func TestLoadOrGenerateKey_CreatesDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep")

	key, err := LoadOrGenerateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if key.IsZero() {
		t.Fatal("key is zero")
	}
}

func TestPublicKeyToTunnelURL(t *testing.T) {
	key, err := tunnelsdk.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubKey, err := key.PublicKey()
	if err != nil {
		t.Fatal(err)
	}

	u, err := PublicKeyToTunnelURL(pubKey, "https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Should be a valid URL with a subdomain.
	if u == "" {
		t.Fatal("empty tunnel URL")
	}

	hostname := PublicKeyToHostname(pubKey)
	if hostname == "" {
		t.Fatal("empty hostname")
	}
	// base32hex output for 8 bytes = 13 chars (ceil(8*8/5))
	if len(hostname) < 10 || len(hostname) > 16 {
		t.Fatalf("unexpected hostname length: %d (%s)", len(hostname), hostname)
	}

	expected := "https://" + hostname + ".relay.example.com"
	if u != expected {
		t.Fatalf("expected %s, got %s", expected, u)
	}
}

func TestPublicKeyToHostname_Deterministic(t *testing.T) {
	key, err := tunnelsdk.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubKey, err := key.PublicKey()
	if err != nil {
		t.Fatal(err)
	}

	h1 := PublicKeyToHostname(pubKey)
	h2 := PublicKeyToHostname(pubKey)
	if h1 != h2 {
		t.Fatalf("hostname not deterministic: %s != %s", h1, h2)
	}
}

func TestPublicKeyToHostname_UniquePerKey(t *testing.T) {
	key1, _ := tunnelsdk.GeneratePrivateKey()
	pub1, _ := key1.PublicKey()
	key2, _ := tunnelsdk.GeneratePrivateKey()
	pub2, _ := key2.PublicKey()

	h1 := PublicKeyToHostname(pub1)
	h2 := PublicKeyToHostname(pub2)
	if h1 == h2 {
		t.Fatal("different keys produced same hostname")
	}
}
