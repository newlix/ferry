package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	serviceLabel = "glms"
	launchctlBin = "/bin/launchctl"
)

func printUsage(out io.Writer) {
	fmt.Fprint(out, `glms — local API gateway for the GLM Coding Plan

Usage:
  glms               run the gateway in the foreground (first run asks for a key)
  glms status        show plans, quota and service state
  glms key add       add a coding-plan key (verified, stored in the Keychain)
  glms key remove    remove a key by name
  glms key list      list key names
  glms install       run at login as a LaunchAgent
  glms uninstall     remove the LaunchAgent

Run flags: -config PATH, -listen ADDR, -v (log every request)
`)
}

// cmdRun is the default action and the launchd entry point: it blocks serving
// HTTP until the process is stopped.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "config file")
	listen := fs.String("listen", "", "listen address, overrides config")
	verbose := fs.Bool("v", false, "log every request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	explicitConfig := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})

	cfg, err := LoadConfig(*configPath, explicitConfig)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	store := NewKeyStore(configDir())
	if err := loadKeys(&cfg, store); err != nil {
		return err
	}
	if len(cfg.APIKeys) == 0 {
		if !isTerminal(os.Stdin) {
			return errors.New("no keys yet — run:  glms key add")
		}
		fmt.Println("No Z.AI key yet. Add one now (empty answer skips).")
		if err := cmdKeyAdd(cfg, store, os.Stdin, os.Stdout); err != nil {
			return err
		}
		if err := loadKeys(&cfg, store); err != nil {
			return err
		}
		if len(cfg.APIKeys) == 0 {
			return errors.New("still no key — run:  glms key add")
		}
	}
	return serve(cfg, NewLogger(*verbose))
}

// loadKeys fills cfg.APIKeys in key-store order; the order is the identity
// key indexes are built on, so it must be stable while the gateway runs.
func loadKeys(cfg *Config, store KeyStore) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	cfg.APIKeys = nil
	for _, name := range names {
		secret, err := store.Get(name)
		if err != nil {
			return fmt.Errorf("load key %q: %w", name, err)
		}
		cfg.APIKeys = append(cfg.APIKeys, secret)
	}
	return nil
}

func cmdKeyAdd(cfg Config, store KeyStore, in io.Reader, out io.Writer) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	name := promptDefault(reader, out, "Name", fmt.Sprintf("key%d", len(names)+1))
	if slices.Contains(names, name) {
		return fmt.Errorf("a key named %q already exists (remove it first: glms key remove %s)", name, name)
	}
	fmt.Fprintln(out, "Paste your GLM Coding Plan API key (hidden on a terminal).")
	secret, err := readSecret(reader, isTerminalReader(in))
	if err != nil {
		return err
	}
	if secret == "" {
		return errors.New("empty key, nothing saved")
	}

	m := newUsageMonitor(cfg.Upstreams.Usage, NewNullLogger())
	u, verr := m.fetchKey(secret)
	var ce *consoleError
	switch {
	case verr == nil:
		fmt.Fprintf(out, "Verified: %s, weekly %.0f%% left, expires %s.\n", planLabel(u), u.weeklyRem*100, usageExpiryLabel(u.expiry))
	case errors.As(verr, &ce) && (ce.code == 401 || ce.code == 403):
		return fmt.Errorf("z.ai rejected this key (%s) — nothing saved", ce.msg)
	default:
		fmt.Fprintf(out, "Could not verify this key (%v); saving anyway.\n", verr)
	}
	if err := store.Add(name, secret); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added %q.\n", name)
	restartServiceIfInstalled(out)
	return nil
}

func cmdKeyRemove(store KeyStore, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: glms key remove NAME (see: glms key list)")
	}
	if err := store.Remove(args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %q.\n", args[0])
	restartServiceIfInstalled(os.Stdout)
	return nil
}

func cmdKeyList(store KeyStore) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("No keys yet. Add one:  glms key add")
		return nil
	}
	for _, name := range names {
		fmt.Println(name)
	}
	return nil
}

func cmdInstall(store KeyStore) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		// Installing keyless would bootstrap a service that exits and is
		// restarted by KeepAlive forever.
		return errors.New("no keys yet — add one first:  glms key add")
	}
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}
	plist := plistPath()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(configDir(), "glms.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(plistContent(execPath, logPath, serviceLabel)), 0o644); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	exec.Command(launchctlBin, "bootout", "gui/"+uid+"/"+serviceLabel).Run() // not installed yet: ignore
	if out, err := exec.Command(launchctlBin, "bootstrap", "gui/"+uid, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	time.Sleep(500 * time.Millisecond)
	cfg, err := LoadConfig(defaultConfigPath(), false)
	if err != nil {
		return err
	}
	if healthCheck(cfg.Listen) {
		fmt.Printf("Installed and running at http://%s (logs: %s).\n", cfg.Listen, logPath)
	} else {
		fmt.Printf("Installed; give it a moment, then check:  glms status  (logs: %s)\n", logPath)
	}
	return nil
}

func cmdUninstall() error {
	uid := strconv.Itoa(os.Getuid())
	exec.Command(launchctlBin, "bootout", "gui/"+uid+"/"+serviceLabel).Run()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Removed the LaunchAgent.")
	return nil
}

// restartServiceIfInstalled applies key changes without making the user
// think about launchctl.
func restartServiceIfInstalled(out io.Writer) {
	if !serviceInstalled() {
		fmt.Fprintln(out, "Not running as a service. Start it at login with:  glms install")
		return
	}
	uid := strconv.Itoa(os.Getuid())
	if out2, err := exec.Command(launchctlBin, "kickstart", "-k", "gui/"+uid+"/"+serviceLabel).CombinedOutput(); err != nil {
		fmt.Fprintf(out, "Could not restart the service (%v: %s); restart it with:  launchctl kickstart -k gui/%s/%s\n", err, out2, uid, serviceLabel)
		return
	}
	fmt.Fprintln(out, "Service restarted.")
}

// serviceInstalled is a var so tests can pin the answer regardless of the
// machine's LaunchAgents directory.
var serviceInstalled = func() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

func plistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "glms.plist"
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
}

// plistContent is split out so its shape is testable without touching launchd.
func plistContent(execPath, logPath, label string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, label, execPath, logPath, logPath)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// isTerminalReader hides typed input only when the reader itself is the
// terminal; piped input (tests, scripts) reads plainly.
func isTerminalReader(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && isTerminal(f)
}

func promptDefault(r *bufio.Reader, out io.Writer, label, def string) string {
	fmt.Fprintf(out, "%s [%s]: ", label, def)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	if s := strings.TrimSpace(line); s != "" {
		return s
	}
	return def
}

// readSecret hides the typed key on a terminal via bash read -s, the one
// stdlib-only way to disable echo; piped input (tests, scripts) reads plainly.
func readSecret(r *bufio.Reader, hidden bool) (string, error) {
	if hidden {
		out, err := exec.Command("/bin/bash", "-c", `read -s -r -p "API key: " k; printf '\n'; printf '%s' "$k"`).Output()
		if err != nil {
			return "", fmt.Errorf("read key: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
