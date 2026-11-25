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
	"syscall"
	"time"
)

const (
	serviceLabel = "ferry"
	launchctlBin = "/bin/launchctl"
)

const usageText = `ferry — local API gateway for the GLM Coding Plan

Usage:
  ferry               run the gateway in the foreground (first run asks for a key)
  ferry status        show keys, plans, quota and running state
  ferry key add       add a GLM Coding Plan key (verified, stored in the Keychain)
		ferry key remove    remove a key by name
  ferry route         show routing; fix it to one key or back to automatic
  ferry install       start ferry at login
  ferry uninstall     stop starting at login

Run flags: -v (log every request)
`

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
			return errors.New("no keys yet. Add one:  ferry key add")
		}
		fmt.Println("No GLM Coding Plan key yet. Add one now.")
		if err := cmdKeyAdd(cfg, store, os.Stdin, os.Stdout, false); err != nil {
			return err
		}
		if serviceInstalled() {
			// The login service loads the key on its next restart; serving
			// here would only fight it for the port.
			fmt.Println("ferry starts at login and now has a key. Check it:  ferry status")
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
	return resolveRoute(cfg)
}

// resolveRoute turns cfg.Route into an index into cfg.APIKeys; an empty
// Route means automatic routing.
func resolveRoute(cfg *Config) error {
	if cfg.Route == "" {
		cfg.RouteIdx = -1
		return nil
	}
	for i, k := range cfg.APIKeys {
		if k.Name == cfg.Route {
			cfg.RouteIdx = i
			return nil
		}
	}
	return fmt.Errorf("config: route %q names no key in the store (see: ferry status)", cfg.Route)
}

// cmdKeyAdd asks for the key first (what the user came holding) and the name
// after verification, when the plan it shows can inspire the name.
func cmdKeyAdd(cfg Config, store KeyStore, in io.Reader, out io.Writer, suggestInstall bool) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	if _, err := fmt.Fprintln(out, "Paste your GLM Coding Plan key (hidden while typing)."); err != nil {
		return err
	}
	secret, err := readSecret(reader, terminalReader(in))
	if err != nil {
		return err
	}
	if secret == "" {
		return errors.New("no key entered, nothing saved. Add one with:  ferry key add")
	}

	m := newUsageMonitor(cfg.Upstreams.Usage, NewNullLogger())
	u, verr := m.fetchKey(secret)
	var ce *consoleError
	switch {
	case verr == nil:
		if _, err := fmt.Fprintf(out, "Verified: %s, %.0f%% left this week (resets in %s).\n", planLabel(u), u.weeklyRem*100, relTime(time.Until(u.weeklyReset))); err != nil {
			return err
		}
	case errors.As(verr, &ce) && (ce.code == 401 || ce.code == 403):
		return fmt.Errorf("z.ai rejected this key (%s). Nothing saved", ce.msg)
	default:
		if _, err := fmt.Fprintf(out, "Could not verify this key (%v); saving anyway.\n", verr); err != nil {
			return err
		}
	}

	name, err := promptDefault(reader, out, "Name this key", defaultKeyName(names))
	if err != nil {
		return err
	}
	if slices.Contains(names, name) {
		return fmt.Errorf("a key named %q already exists (remove it first: ferry key remove %s)", name, name)
	}
	if err := store.Add(name, secret); err != nil {
		return err
	}
	// The key is saved: a failed write of this line must not skip telling
	// the running ferry about it.
	_, werr := fmt.Fprintf(out, "Added %q.\n", name)
	return errors.Join(werr, applyKeyChange(cfg, out, suggestInstall))
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
		return errors.New("usage: ferry key remove NAME (see: ferry status)")
	}
	names, err := store.Names()
	if err != nil {
		return err
	}
	if !slices.Contains(names, args[0]) {
		return fmt.Errorf("no key named %q (see:  ferry status)", args[0])
	}
	// A load error leaves the route question open; removal proceeds and the
	// startup route check fails loudly if the name really was pinned.
	loaded, loadErr := LoadConfig(defaultConfigPath())
	if loadErr == nil && loaded.Route == args[0] {
		return fmt.Errorf("%q is the fixed route. Unpin first:  ferry route auto", args[0])
	}
	if serviceInstalled() && len(names) == 1 {
		return fmt.Errorf("%q is the last key, and ferry starts at login. Add another key first or stop starting at login:  ferry uninstall", args[0])
	}
	if err := store.Remove(args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %q.\n", args[0])
	cfg := loaded
	if loadErr != nil {
		cfg = Config{Listen: defaultListen}
	}
	return applyKeyChange(cfg, os.Stdout, false)
}

// cmdRoute shows routing, fixes it to one key, or returns to automatic.
// auto names no key: a key literally named "auto" cannot be picked, so
// choose such a key another name.
func cmdRoute(cfg Config, store KeyStore, path string, args []string, out io.Writer) error {
	if len(args) == 0 {
		header := "Routing is automatic: keys share the traffic, each conversation stays on one key."
		if cfg.Route != "" {
			header = fmt.Sprintf("Routing is fixed to %s.", cfg.Route)
		}
		if _, err := fmt.Fprintln(out, header); err != nil {
			return err
		}
		names, err := store.Names()
		if err != nil {
			return err
		}
		var choices strings.Builder
		for _, name := range names {
			fmt.Fprintf(&choices, "  ferry route %-14s fix every request to this key\n", name)
		}
		choices.WriteString("  ferry route auto           share traffic across keys\n")
		_, err = io.WriteString(out, choices.String())
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: ferry route [NAME | auto]")
	}
	name := args[0]
	if name != "auto" {
		names, err := store.Names()
		if err != nil {
			return err
		}
		if !slices.Contains(names, name) {
			return fmt.Errorf("no key named %q (see:  ferry status)", name)
		}
	}
	if err := writeRoute(path, routeConfigValue(name)); err != nil {
		return err
	}
	done := fmt.Sprintf("Every request now uses %s.", name)
	if name == "auto" {
		done = "Routing is automatic: keys share the traffic, each conversation stays on one key."
	}
	// The route is written: a failed write of this line must not skip
	// telling the running ferry about it.
	_, werr := fmt.Fprintln(out, done)
	return errors.Join(werr, applyKeyChange(cfg, out, false))
}

// routeConfigValue maps the CLI keyword to the config field; the config
// field is empty for automatic routing.
func routeConfigValue(name string) string {
	if name == "auto" {
		return ""
	}
	return name
}

func cmdInstall(store KeyStore) error {
	names, err := store.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		// Installing keyless would bootstrap a service that exits and is
		// restarted by KeepAlive forever.
		return errors.New("no keys yet. Add one:  ferry key add")
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
	logPath := filepath.Join(configDir(), "ferry.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(plistContent(execPath, logPath, serviceLabel)), 0o644); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	domain := "gui/" + uid
	if err := bootout(domain, serviceLabel); err != nil {
		return err
	}
	if out, err := exec.Command(launchctlBin, "bootstrap", domain, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("could not start ferry at login: %v: %s", err, out)
	}
	time.Sleep(500 * time.Millisecond)
	cfg, err := LoadConfig(defaultConfigPath())
	if err != nil {
		return err
	}
	if healthCheck(cfg.Listen) {
		fmt.Printf("Installed and running at http://%s (logs: %s).\n", cfg.Listen, logPath)
	} else {
		fmt.Printf("Installed; give it a moment, then check:  ferry status  (logs: %s)\n", logPath)
	}
	return nil
}

func cmdUninstall() error {
	uid := strconv.Itoa(os.Getuid())
	if err := bootout("gui/"+uid, serviceLabel); err != nil {
		return err
	}
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("ferry no longer starts at login.")
	return nil
}

// bootout stops the service if launchd has it loaded. launchctl bootout
// fails when the service is not loaded, the usual case on a first install, so
// its exit status is not the answer: the service still being known after the
// wait is, and only then is its failure returned.
func bootout(domain, label string) error {
	out, err := exec.Command(launchctlBin, "bootout", domain+"/"+label).CombinedOutput()
	if waitServiceGone(domain, label, 3*time.Second) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("launchctl bootout %s/%s: %w: %s", domain, label, err, out)
	}
	return fmt.Errorf("launchctl bootout %s/%s: still loaded after 3s", domain, label)
}

// waitServiceGone reports whether launchd stopped knowing the service within
// timeout: bootout is asynchronous, and bootstrapping while the old instance
// is still tearing down fails with "Input/output error".
func waitServiceGone(domain, label string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := exec.Command(launchctlBin, "print", domain+"/"+label).Run(); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// applyKeyChange makes a key or routing change reach the running ferry:
// launchd restarts the service, everything else SIGHUPs the running process,
// which re-reads config and keys (see serve's reload handler). suggestInstall
// is false on first run and after a removal or route change: the install
// hint would interrupt the flow the user is already in.
func applyKeyChange(cfg Config, out io.Writer, suggestInstall bool) error {
	if serviceInstalled() {
		uid := strconv.Itoa(os.Getuid())
		if err := exec.Command(launchctlBin, "kickstart", "-k", "gui/"+uid+"/"+serviceLabel).Run(); err != nil {
			_, werr := fmt.Fprintf(out, "Could not restart ferry; the change takes effect at your next login (logs: %s)\n", filepath.Join(configDir(), "ferry.log"))
			return werr
		}
		_, err := fmt.Fprintln(out, "Restarted ferry.")
		return err
	}
	var werr error
	if suggestInstall {
		// A failed hint must not skip the reload below.
		_, werr = fmt.Fprintln(out, "Not set to start at login. Set it with:  ferry install")
	}
	return errors.Join(werr, reloadRunning(cfg, out))
}

// reloadRunning signals the ferry recorded in the pidfile. A missing or
// stale pidfile — an older ferry, or one running elsewhere — keeps the
// restart hint.
func reloadRunning(cfg Config, out io.Writer) error {
	pid, ok := runningFerryPid()
	if !ok {
		if healthCheck(cfg.Listen) {
			_, err := fmt.Fprintln(out, "ferry is running in another terminal; restart it to pick this up.")
			return err
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		_, werr := fmt.Fprintln(out, "Could not signal the running ferry; restart it to pick this up.")
		return werr
	}
	_, err := fmt.Fprintf(out, "Signaled ferry (pid %d) to reload config and keys.\n", pid)
	return err
}

// runningFerryPid returns the pidfile pid when it still names a ferry
// process, so a reused pid never receives our signal.
func runningFerryPid() (int, bool) {
	data, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	comm, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(comm)), serviceLabel) {
		return 0, false
	}
	return pid, true
}

func pidPath() string { return filepath.Join(configDir(), "ferry.pid") }

func writePidFile() error {
	return os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// serviceInstalled is a var so tests can pin the answer regardless of the
// machine's LaunchAgents directory.
var serviceInstalled = func() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

func plistPathNamed(label string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return label + ".plist"
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func plistPath() string { return plistPathNamed(serviceLabel) }

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

// terminalReader returns in as a file when it is an interactive terminal,
// else nil.
func terminalReader(in io.Reader) *os.File {
	f, ok := in.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	return f
}

func promptDefault(r *bufio.Reader, out io.Writer, label, def string) (string, error) {
	if _, err := fmt.Fprintf(out, "%s [%s]: ", label, def); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return def, nil
	}
	if s := strings.TrimSpace(line); s != "" {
		return s, nil
	}
	return def, nil
}

// readSecret hides the typed key when hiddenFrom is a terminal: bash read -s
// is the one stdlib-only way to disable echo. The terminal itself must be the
// bash process's stdin: exec without Stdin reads /dev/null, which ends the
// read at once with an empty key. Piped input (tests, scripts) reads plainly.
func readSecret(r *bufio.Reader, hiddenFrom *os.File) (string, error) {
	if hiddenFrom != nil {
		cmd := exec.Command("/bin/bash", "-c", `read -s -r -p "API key: " k; printf '\n'; printf '%s' "$k"`)
		cmd.Stdin = hiddenFrom
		out, err := cmd.Output()
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
