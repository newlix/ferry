package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Keys live outside the config file: names in an index file under the config
// dir, secrets in the macOS Keychain via /usr/bin/security. On other systems
// the secrets fall back to a 0600 file, keeping the tool dependency-free.

type KeyStore interface {
	Names() ([]string, error)
	Get(name string) (string, error)
	Add(name, secret string) error
	Remove(name string) error
}

func NewKeyStore(dir string) KeyStore {
	if runtime.GOOS == "darwin" {
		return &keychainStore{dir: dir, service: "ferry"}
	}
	return &fileStore{dir: dir}
}

// fileStore keeps everything in keys.json (0600). Test baseline and
// non-darwin fallback.
type fileStore struct {
	dir string
}

func (s *fileStore) path() string { return filepath.Join(s.dir, "keys.json") }

func (s *fileStore) load() (map[string]string, error) {
	secrets := map[string]string{}
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return secrets, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path(), err)
	}
	return secrets, nil
}

func (s *fileStore) save(secrets map[string]string) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), data, 0o600)
}

func (s *fileStore) Names() ([]string, error) {
	secrets, err := s.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

func (s *fileStore) Get(name string) (string, error) {
	secrets, err := s.load()
	if err != nil {
		return "", err
	}
	secret, ok := secrets[name]
	if !ok {
		return "", fmt.Errorf("no key named %q", name)
	}
	return secret, nil
}

func (s *fileStore) Add(name, secret string) error {
	secrets, err := s.load()
	if err != nil {
		return err
	}
	secrets[name] = secret
	return s.save(secrets)
}

func (s *fileStore) Remove(name string) error {
	secrets, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := secrets[name]; !ok {
		return fmt.Errorf("no key named %q", name)
	}
	delete(secrets, name)
	return s.save(secrets)
}

// keychainStore keeps names in an index file and secrets as generic
// passwords in the login keychain. Only the index file is tested here; the
// security calls themselves are exercised the first time a real key is added.
type keychainStore struct {
	dir     string
	service string
}

func (s *keychainStore) indexPath() string { return filepath.Join(s.dir, "keys.json") }

func (s *keychainStore) Names() ([]string, error) {
	var index struct {
		Names []string `json:"names"`
	}
	data, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.indexPath(), err)
	}
	return index.Names, nil
}

func (s *keychainStore) saveNames(names []string) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Names []string `json:"names"`
	}{names}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath(), data, 0o600)
}

func (s *keychainStore) Get(name string) (string, error) {
	out, err := exec.Command("/usr/bin/security", securityArgs("find", s.service, name, "")...).Output()
	if err != nil {
		return "", fmt.Errorf("keychain read %q: %w", name, err)
	}
	secret := strings.TrimSpace(string(out))
	if secret == "" {
		return "", fmt.Errorf("keychain read %q: empty secret", name)
	}
	return secret, nil
}

// Add passes the secret on the argv of /usr/bin/security: it is briefly
// visible to processes of the same user. The security CLI has no practical
// stdin-only mode, and the fileStore alternative writes it to disk.
func (s *keychainStore) Add(name, secret string) error {
	cmd := exec.Command("/usr/bin/security", securityArgs("add", s.service, name, secret)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain write %q: %v: %s", name, err, out)
	}
	names, err := s.Names()
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		names = append(names, name)
	}
	return s.saveNames(names)
}

func (s *keychainStore) Remove(name string) error {
	if out, err := exec.Command("/usr/bin/security", securityArgs("delete", s.service, name, "")...).CombinedOutput(); err != nil {
		return fmt.Errorf("keychain delete %q: %v: %s", name, err, out)
	}
	names, err := s.Names()
	if err != nil {
		return err
	}
	if i := slices.Index(names, name); i >= 0 {
		names = slices.Delete(names, i, i+1)
	}
	return s.saveNames(names)
}

// securityArgs builds the argv for the one security invocation per
// operation, split out so it can be tested without touching the keychain.
func securityArgs(op, service, name, secret string) []string {
	switch op {
	case "add":
		return []string{"add-generic-password", "-s", service, "-a", name, "-w", secret, "-U"}
	case "find":
		return []string{"find-generic-password", "-s", service, "-a", name, "-w"}
	case "delete":
		return []string{"delete-generic-password", "-s", service, "-a", name}
	}
	panic("unknown security op " + op)
}
