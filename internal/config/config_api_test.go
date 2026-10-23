package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validHash = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

func loadJSON(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestAPIKeysValid(t *testing.T) {
	// Upper-case hex must be accepted and normalized to lower-case.
	upper := validHash[:7] + strings.ToUpper(validHash[7:])
	c, err := loadJSON(t, `{"api":{"keys":[{"name":"bi","hash":"`+upper+`","scopes":["bookings:read"]}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.API.Keys) != 1 || c.API.Keys[0].Hash != validHash {
		t.Fatalf("hash not normalized: %+v", c.API.Keys)
	}
}

func TestAPIKeysNoneIsFine(t *testing.T) {
	if _, err := loadJSON(t, `{}`); err != nil {
		t.Fatal(err)
	}
}

func TestAPIKeysInvalid(t *testing.T) {
	other := "sha256:" + strings.Repeat("a", 64)
	cases := map[string]string{
		"missing name":   `{"api":{"keys":[{"hash":"` + validHash + `","scopes":["x"]}]}}`,
		"short hash":     `{"api":{"keys":[{"name":"a","hash":"sha256:abc","scopes":["x"]}]}}`,
		"empty hash":     `{"api":{"keys":[{"name":"a","hash":"","scopes":["x"]}]}}`,
		"non-hex hash":   `{"api":{"keys":[{"name":"a","hash":"sha256:` + strings.Repeat("z", 64) + `","scopes":["x"]}]}}`,
		"no scopes":      `{"api":{"keys":[{"name":"a","hash":"` + validHash + `"}]}}`,
		"duplicate name": `{"api":{"keys":[{"name":"a","hash":"` + validHash + `","scopes":["x"]},{"name":"a","hash":"` + other + `","scopes":["x"]}]}}`,
		"duplicate hash": `{"api":{"keys":[{"name":"a","hash":"` + validHash + `","scopes":["x"]},{"name":"b","hash":"` + validHash + `","scopes":["x"]}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadJSON(t, body); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
