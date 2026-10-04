package main

// The supersearch CLI: this same binary installed as `supersearch` (see
// install.sh). It runs a long-lived server that phones, tablets, other
// computers, or Obsidian itself connect to with a URL and a token. The token
// and the index live in ~/.config/supersearch, so restarts keep both.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const defaultListen = "127.0.0.1:54999"

const cliUsage = `supersearch: fast search for your Obsidian vault, as a server you connect to

Usage:
  supersearch [vault]          start the server (the vault is remembered)
  supersearch serve [flags]    same, with flags:
      -vault PATH              Obsidian vault to index
      -listen HOST:PORT        address (default 127.0.0.1:54999)
      -lan                     listen on every network interface (home Wi-Fi)
      -ocr-workers N           parallel OCR jobs (default 3)
  supersearch vault [PATH]     print or set the default vault
  supersearch token [-new]     print the token (-new: replace it)
  supersearch status           indexing progress of the running server
  supersearch search WORDS     search through the running server
  supersearch help

Files: ~/.config/supersearch (token, config.json, indexes). Override with
SUPERSEARCH_CONFIG_DIR. SUPERSEARCH_TOKEN, if set, wins over the saved token.
`

// cliMode: the binary was started as "supersearch", not "supersearch-server".
func cliMode() bool {
	name := strings.ToLower(filepath.Base(os.Args[0]))
	return strings.TrimSuffix(name, ".exe") == "supersearch"
}

// configDir holds the CLI's token, settings and indexes (and, on Linux and
// Windows, the speech model).
func configDir() string {
	if d := os.Getenv("SUPERSEARCH_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "supersearch")
}

type cliConfig struct {
	Vault  string `json:"vault,omitempty"`
	Listen string `json:"listen,omitempty"`
}

func loadCLIConfig() cliConfig {
	var c cliConfig
	if b, err := os.ReadFile(filepath.Join(configDir(), "config.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func (c cliConfig) save() error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(configDir(), "config.json"), append(b, '\n'), 0o600)
}

// cliToken returns the saved token, creating it on first use. The same token
// comes back on every run until `supersearch token -new`.
func cliToken(rotate bool) (string, error) {
	if t := strings.TrimSpace(os.Getenv("SUPERSEARCH_TOKEN")); t != "" && !rotate {
		return t, nil
	}
	file := filepath.Join(configDir(), "token")
	if !rotate {
		if b, err := os.ReadFile(file); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, nil
			}
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return "", err
	}
	return t, os.WriteFile(file, []byte(t+"\n"), 0o600)
}

// vaultDB keeps one index per vault: <config>/vaults/<name>-<hash>/index.db.
func vaultDB(vault string) string {
	sum := sha256.Sum256([]byte(vault))
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r < ' ' {
			return '_'
		}
		return r
	}, filepath.Base(vault))
	return filepath.Join(configDir(), "vaults", name+"-"+hex.EncodeToString(sum[:4]), "index.db")
}

func cli(args []string) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "serve", "vault", "token", "status", "search", "help", "version":
			cmd, args = args[0], args[1:]
		}
	}
	var err error
	switch cmd {
	case "serve":
		err = cliServe(args)
	case "token":
		fs := flag.NewFlagSet("token", flag.ContinueOnError)
		rotate := fs.Bool("new", false, "replace the token (restart the server and update your devices)")
		if err = fs.Parse(args); err == nil {
			var t string
			if t, err = cliToken(*rotate); err == nil {
				fmt.Println(t)
				if *rotate {
					fmt.Fprintln(os.Stderr, "new token saved: restart supersearch and paste it into every device")
				}
			}
		}
	case "vault":
		err = cliVault(args)
	case "status":
		err = cliStatus()
	case "search":
		err = cliSearch(strings.Join(args, " "))
	case "version":
		fmt.Printf("supersearch (%s/%s, %s backend)\n", runtime.GOOS, runtime.GOARCH, map[bool]string{true: "Apple", false: "portable"}[appleBackend])
	default:
		fmt.Print(cliUsage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "supersearch:", err)
		return 1
	}
	return 0
}

func cliServe(args []string) error {
	cfg := loadCLIConfig()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	vault := fs.String("vault", "", "Obsidian vault to index")
	listen := fs.String("listen", "", "address to serve on (default "+defaultListen+")")
	lan := fs.Bool("lan", false, "listen on every network interface")
	ocrWorkers := fs.Int("ocr-workers", 3, "parallel OCR jobs on mains power (1 on battery)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 && *vault == "" {
		*vault = fs.Arg(0)
	}
	changed := *vault != "" || *listen != ""
	if *vault == "" {
		*vault = cfg.Vault
	}
	if *listen == "" {
		*listen = cfg.Listen
	}
	if *listen == "" {
		*listen = defaultListen
	}
	if *lan {
		_, port, err := net.SplitHostPort(*listen)
		if err != nil {
			return fmt.Errorf("-listen %q: %w", *listen, err)
		}
		*listen = net.JoinHostPort("0.0.0.0", port)
	}
	if *vault == "" {
		return errors.New("which vault? run: supersearch /path/to/vault (it is remembered)")
	}
	abs, err := filepath.Abs(*vault)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("vault not found: %s", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, ".obsidian")); err != nil {
		fmt.Fprintf(os.Stderr, "note: %s has no .obsidian folder; is it an Obsidian vault?\n", abs)
	}
	if changed {
		cfg.Vault = abs
		if !*lan {
			cfg.Listen = *listen
		}
		if err := cfg.save(); err != nil {
			return err
		}
	}
	token, err := cliToken(false)
	if err != nil {
		return err
	}
	db := vaultDB(abs)
	ix, err := openVault(abs, db)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "in use") || strings.Contains(msg, "Only one usage") {
			return fmt.Errorf("%s is already in use: is supersearch already running? (supersearch status), or pick another port with -listen", *listen)
		}
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	printBanner(abs, db, *listen, port, token)
	go reportProgress(ix)
	return serve(ix, ln, *ocrWorkers, token)
}

func cliVault(args []string) error {
	cfg := loadCLIConfig()
	if len(args) == 0 {
		if cfg.Vault == "" {
			return errors.New("no default vault yet: supersearch vault /path/to/vault")
		}
		fmt.Println(cfg.Vault)
		return nil
	}
	abs, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("vault not found: %s", abs)
	}
	cfg.Vault = abs
	return cfg.save()
}

func printBanner(vault, db, listen string, port int, token string) {
	host, _, _ := net.SplitHostPort(listen)
	all := host == "" || host == "0.0.0.0" || host == "::"
	local := host
	if all {
		local = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(local, strconv.Itoa(port))
	fmt.Printf("\nSupersearch is running\n\n")
	fmt.Printf("  Vault   %s\n", vault)
	fmt.Printf("  URL     %s\n", url)
	if all {
		for _, ip := range lanIPs() {
			fmt.Printf("          http://%s\n", net.JoinHostPort(ip, strconv.Itoa(port)))
		}
	}
	fmt.Printf("  Token   %s\n", token)
	fmt.Printf("  Index   %s\n\n", filepath.Dir(db))
	fmt.Printf("In Obsidian: Settings → Supersearch → Server URL and Server token.\n")
	if ts := tailscaleName(); ts != "" {
		fmt.Printf("Other devices: run  tailscale serve --bg %d  once, then use https://%s\n", port, ts)
	} else if !all {
		fmt.Printf("Other devices: use Tailscale (tailscale serve --bg %d), or restart with -lan for your home network.\n", port)
	}
	fmt.Printf("Stop with Ctrl+C.\n\n")
}

// reportProgress logs the indexing backlog every 30 s while there is one.
func reportProgress(ix *Index) {
	busy := true
	for {
		time.Sleep(5 * time.Second)
		s, err := ix.status()
		if err != nil {
			continue
		}
		c := s["counts"].(map[string]int)
		left := c["pending"] + c["ocr"]
		switch {
		case left > 0:
			fmt.Printf("indexing: %d files left (%d done)\n", left, c["done"])
			busy = true
			time.Sleep(25 * time.Second)
		case busy:
			fmt.Printf("index up to date: %d files", c["done"])
			if c["error"] > 0 {
				fmt.Printf(", %d unreadable", c["error"])
			}
			fmt.Println()
			if m := s["missing"].([]string); len(m) > 0 {
				fmt.Printf("missing tools: %s\n", strings.Join(m, ", "))
			}
			busy = false
		}
	}
}

func lanIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP.String())
		}
	}
	return out
}

// tailscaleName is this machine's MagicDNS name, or "" without Tailscale.
func tailscaleName() string {
	bin := ""
	for _, c := range []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		filepath.Join(os.Getenv("ProgramFiles"), "Tailscale", "tailscale.exe")} {
		if p, err := exec.LookPath(c); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := command(ctx, false, bin, "status", "--json").Output()
	if err != nil {
		return ""
	}
	var st struct{ Self struct{ DNSName string } }
	json.Unmarshal(out, &st)
	return strings.TrimSuffix(st.Self.DNSName, ".")
}

// running talks to the CLI's server on this machine.
func running(path string) ([]byte, error) {
	listen := loadCLIConfig().Listen
	if listen == "" {
		listen = defaultListen
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	token, err := cliToken(false)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "http://"+net.JoinHostPort("127.0.0.1", port)+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("no server on port %s: start it with: supersearch", port)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("server: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func cliStatus() error {
	b, err := running("/status")
	if err != nil {
		return err
	}
	var s struct {
		Counts  map[string]int
		Paused  bool
		Missing []string
	}
	json.Unmarshal(b, &s)
	c := s.Counts
	fmt.Printf("vault:   %s\n", loadCLIConfig().Vault)
	fmt.Printf("indexed: %d files", c["done"])
	if left := c["pending"] + c["ocr"]; left > 0 {
		fmt.Printf(", %d left", left)
		if s.Paused {
			fmt.Print(" (paused)")
		}
	}
	if c["error"] > 0 {
		fmt.Printf(", %d unreadable", c["error"])
	}
	fmt.Println()
	if len(s.Missing) > 0 {
		fmt.Printf("missing: %s\n", strings.Join(s.Missing, ", "))
	}
	return nil
}

func cliSearch(q string) error {
	if strings.TrimSpace(q) == "" {
		return errors.New("usage: supersearch search WORDS")
	}
	b, err := running("/search?limit=20&q=" + url.QueryEscape(q))
	if err != nil {
		return err
	}
	var res struct {
		Results   []Result
		Corrected string
		Ms        float64
	}
	json.Unmarshal(b, &res)
	if res.Corrected != "" {
		fmt.Printf("showing results for: %s\n", res.Corrected)
	}
	for _, r := range res.Results {
		loc := r.Path
		if r.Page > 0 {
			loc += fmt.Sprintf(" (page %d)", r.Page)
		} else if r.Line > 0 {
			loc += fmt.Sprintf(":%d", r.Line+1)
		}
		snippet := strings.NewReplacer("\x02", "[", "\x03", "]", "\n", " ").Replace(r.Snippet)
		fmt.Printf("%s\n    %s\n", loc, strings.Join(strings.Fields(snippet), " "))
	}
	fmt.Printf("\n%d results in %.1f ms\n", len(res.Results), res.Ms)
	return nil
}
