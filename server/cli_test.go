package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLITokenPersists(t *testing.T) {
	t.Setenv("SUPERSEARCH_CONFIG_DIR", t.TempDir())
	t.Setenv("SUPERSEARCH_TOKEN", "")
	a, err := cliToken(false)
	if err != nil || len(a) != 64 {
		t.Fatalf("token %q %v", a, err)
	}
	if b, _ := cliToken(false); b != a {
		t.Errorf("second run gave a different token: %q vs %q", b, a)
	}
	if st, err := os.Stat(filepath.Join(configDir(), "token")); err != nil || (st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Errorf("token file must be private: %v %v", st, err)
	}
	c, _ := cliToken(true)
	if c == a {
		t.Error("-new kept the old token")
	}
	if d, _ := cliToken(false); d != c {
		t.Error("rotated token not saved")
	}
	t.Setenv("SUPERSEARCH_TOKEN", "from-env")
	if e, _ := cliToken(false); e != "from-env" {
		t.Errorf("SUPERSEARCH_TOKEN must win, got %q", e)
	}
}

func TestCLIConfigAndDB(t *testing.T) {
	t.Setenv("SUPERSEARCH_CONFIG_DIR", t.TempDir())
	if (cliConfig{Vault: "/v", Listen: "127.0.0.1:1"}).save() != nil {
		t.Fatal("save")
	}
	if c := loadCLIConfig(); c.Vault != "/v" || c.Listen != "127.0.0.1:1" {
		t.Errorf("config round trip: %+v", c)
	}
	a, b := vaultDB("/notes/Work"), vaultDB("/other/Work")
	if a == b || !strings.Contains(a, "Work-") || !strings.HasPrefix(a, configDir()) {
		t.Errorf("one index per vault under the config dir: %s %s", a, b)
	}
}

func TestURIPath(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/Mobile Documents/index.db": "/Users/me/Mobile%20Documents/index.db",
		`C:\Users\me\vault\index.db`:          "/C:/Users/me/vault/index.db",
	} {
		if os.PathSeparator == '/' && strings.Contains(in, `\`) {
			// filepath.ToSlash only converts the native separator.
			in = strings.ReplaceAll(in, `\`, "/")
		}
		if got := uriPath(in); got != want {
			t.Errorf("uriPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRTF(t *testing.T) {
	doc := `{\rtf1\ansi\ansicpg1252\deff0{\fonttbl{\f0 Helvetica;}}{\colortbl;\red0\green0\blue0;}
{\*\generator Riched20;}\f0\fs24 Caf\'e9 \b quokka\b0  notes\par
Second line with \{braces\} and \u8364?uro and a\tab tab.\par
{\info{\title hidden title}}}`
	got := rtfText([]byte(doc))
	for _, want := range []string{"Café quokka notes\n", "Second line with {braces} and €uro and a\ttab."} {
		if !strings.Contains(got, want) {
			t.Errorf("rtf text %q: missing %q", got, want)
		}
	}
	for _, bad := range []string{"Helvetica", "Riched20", "hidden title", "red0", `\`} {
		if strings.Contains(got, bad) {
			t.Errorf("rtf text %q: leaked %q", got, bad)
		}
	}
}
