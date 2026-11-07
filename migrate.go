package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"time"
)

// One-time migration from the pre-rename glms install (the rename to ferry
// touched the config dir, the Keychain service and the launchd label). Every
// command migrates first; each step is a no-op once nothing of glms is left,
// so this is safe to run any number of times. Remove this file once no
// machine can still hold a glms install.
const glmsName = "glms"

func migrateGlms() error {
	if err := migrateConfigDir(); err != nil {
		return fmt.Errorf("migrate glms config: %w", err)
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	src := &keychainStore{dir: configDir(), service: glmsName}
	if err := copyKeys(src, NewKeyStore(configDir())); err != nil {
		return fmt.Errorf("migrate glms keys: %w", err)
	}
	if err := migrateLaunchd(); err != nil {
		return fmt.Errorf("migrate glms login service: %w", err)
	}
	return nil
}

// migrateConfigDir renames the glms config dir, carrying config.json and
// (off darwin) keys.json. A dir already at the new name wins: ferry has been
// used since the rename and the old dir is stale.
func migrateConfigDir() error {
	old := configDirNamed(glmsName)
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	cur := configDir()
	if _, err := os.Stat(cur); err == nil {
		return nil
	}
	return os.Rename(old, cur)
}

// copyKeys brings every key the old store lists that the current one lacks
// over to the current store. A run interrupted mid-copy completes on the
// next one: a name already in the current store is skipped.
func copyKeys(src, dst KeyStore) error {
	names, err := src.Names()
	if err != nil {
		return err
	}
	existing, err := dst.Names()
	if err != nil {
		return err
	}
	for _, name := range names {
		if slices.Contains(existing, name) {
			continue
		}
		secret, err := src.Get(name)
		if err != nil {
			return fmt.Errorf("read %q: %w", name, err)
		}
		if err := dst.Add(name, secret); err != nil {
			return fmt.Errorf("save %q: %w", name, err)
		}
	}
	return nil
}

// migrateLaunchd removes the old glms login service and installs ferry in its
// place: glms being installed means start-at-login was wanted. Pre-rename
// install refused to run keyless, so a glms.plist implies keys, and copyKeys
// has already brought them over.
func migrateLaunchd() error {
	if _, err := os.Stat(plistPathNamed(glmsName)); err != nil {
		return nil
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	exec.Command(launchctlBin, "bootout", domain+"/"+glmsName).Run()
	waitServiceGone(domain, glmsName, 3*time.Second)
	if err := os.Remove(plistPathNamed(glmsName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return cmdInstall(NewKeyStore(configDir()))
}
