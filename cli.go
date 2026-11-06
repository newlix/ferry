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
  glms status        show keys, plans, quota and running state
  glms key add       add a GLM Coding Plan key (verified, stored in the Keychain)
  glms key remove    remove a key by name
  glms install       start glms at login
  glms uninstall     stop starting at login

Run flags: -v (log every request)
`)
}

// cmdRun is the default action and the launchd entry point: it blocks serving
// HTTP until the process is stopped.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "log every request")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := LoadConfig(defaultConfigPath())
	if err != nil {
		return err
	}

	store := NewKeyStore(configDir())
	if err := loadKeys(&cfg, store); err != nil {
		return err
	}
	if len(cfg.APIKeys) == 0 {
		if !isTerminal(os.Stdin) {
			return errors.New("no keys yet. Add one:  glms key add")
		}
		fmt.Println("No GLM Coding Plan key yet. Add one now.")
		if err := cmdKeyAdd(cfg, store, os.Stdin, os.Stdout, false); err != nil {
			return err
		}
		if serviceInstalled() {
			// The login service loads the key on its next restart; serving
			// here would only fight it for the port.
			fmt.Println("glms starts at login and now has a key. Check it:  glms status")
			return nil
		}
		if err := loadKeys(&cfg, store); err != nil {
			return err
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
		cfg.APIKeys = append(cfg.APIKeys, apiKey{Name: name, Secret: secret})
	}
	return nil
}

// cmdKeyAdd asks for the key first (what the user came holding) and the name
// after verification, when the plan it shows can inspire the name.
func cmdKeyAdd(cfg Config, store KeyStore, in io.Reader, out io.Writer, suggestInstall bool) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	fmt.Fprintln(out, "Paste your GLM Coding Plan key (hidden while typing).")
	secret, err := readSecret(reader, isTerminalReader(in))
	if err != nil {
		return err
	}
	if secret == "" {
		return errors.New("no key entered, nothing saved. Add one with:  glms key add")
	}

	m := newUsageMonitor(cfg.Upstreams.Usage, NewNullLogger())
	u, verr := m.fetchKey(secret)
	var ce *consoleError
	switch {
	case verr == nil:
		fmt.Fprintf(out, "Verified: %s, %.0f%% left this week (resets in %s).\n", planLabel(u), u.weeklyRem*100, relTime(time.Until(u.weeklyReset)))
	case errors.As(verr, &ce) && (ce.code == 401 || ce.code == 403):
		return fmt.Errorf("z.ai rejected this key (%s). Nothing saved", ce.msg)
	default:
		fmt.Fprintf(out, "Could not verify this key (%v); saving anyway.\n", verr)
	}

	name := promptDefault(reader, out, "Name this key", defaultKeyName(names))
	if slices.Contains(names, name) {
		return fmt.Errorf("a key named %q already exists (remove it first: glms key remove %s)", name, name)
	}
	if err := store.Add(name, secret); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added %q.\n", name)
	applyKeyChange(out, suggestInstall)
	return nil
}

// defaultKeyName picks the first unused keyN so accepting the default can
// never collide (len(names)+1 would, after a removal).
func defaultKeyName(names []string) string {
	for i := 1; ; i++ {
		if name := fmt.Sprintf("key%d", i); !slices.Contains(names, name) {
			return name
		}
	}
}

func cmdKeyRemove(store KeyStore, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: glms key remove NAME (see: glms status)")
	}
	names, err := store.Names()
	if err != nil {
		return err
	}
	if !slices.Contains(names, args[0]) {
		return fmt.Errorf("no key named %q (see:  glms status)", args[0])
	}
	if serviceInstalled() && len(names) == 1 {
		return fmt.Errorf("%q is the last key, and glms starts at login. Add another key first or stop starting at login:  glms uninstall", args[0])
	}
	if err := store.Remove(args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %q.\n", args[0])
	applyKeyChange(os.Stdout, false)
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
		return errors.New("no keys yet. Add one:  glms key add")
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
	domain := "gui/" + uid
	exec.Command(launchctlBin, "bootout", domain+"/"+serviceLabel).Run()
	waitServiceGone(domain, 3*time.Second)
	if out, err := exec.Command(launchctlBin, "bootstrap", domain, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("could not start glms at login: %v: %s", err, out)
	}
	time.Sleep(500 * time.Millisecond)
	cfg, err := LoadConfig(defaultConfigPath())
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
	fmt.Println("glms no longer starts at login.")
	return nil
}

// waitServiceGone blocks until launchd no longer knows the service: bootout
// is asynchronous, and bootstrapping while the old instance is still tearing
// down fails with "Input/output error".
func waitServiceGone(domain string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := exec.Command(launchctlBin, "print", domain+"/"+serviceLabel).Run(); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// applyKeyChange restarts an installed glms so a key change takes effect at
// once. suggestInstall is false on first run and after a removal: the install
// hint would interrupt the flow the user is already in.
func applyKeyChange(out io.Writer, suggestInstall bool) {
	if !serviceInstalled() {
		if suggestInstall {
			fmt.Fprintln(out, "Not set to start at login. Set it with:  glms install")
		}
		return
	}
	uid := strconv.Itoa(os.Getuid())
	if err := exec.Command(launchctlBin, "kickstart", "-k", "gui/"+uid+"/"+serviceLabel).Run(); err != nil {
		fmt.Fprintf(out, "Could not restart glms; the new keys load at your next login (logs: %s)\n", filepath.Join(configDir(), "glms.log"))
		return
	}
	fmt.Fprintln(out, "Restarted with the new keys.")
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
