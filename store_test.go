package main

import (
	"os"
	"slices"
	"testing"
)

func TestFileStoreRoundTrip(t *testing.T) {
	s := &fileStore{dir: t.TempDir()}

	names, err := s.Names()
	if err != nil || len(names) != 0 {
		t.Fatalf("Names on empty store = %v, %v", names, err)
	}

	if err := s.Add("pro", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("lite", "sk-2"); err != nil {
		t.Fatal(err)
	}
	names, err = s.Names()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"lite", "pro"}) {
		t.Errorf("Names = %v, want [lite pro]", names)
	}
	for name, want := range map[string]string{"pro": "sk-1", "lite": "sk-2"} {
		got, err := s.Get(name)
		if err != nil || got != want {
			t.Errorf("Get(%q) = %q, %v; want %q", name, got, err, want)
		}
	}

	if err := s.Remove("pro"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("pro"); err == nil {
		t.Error("Get(removed) succeeded, want error")
	}
	if err := s.Remove("pro"); err == nil {
		t.Error("double Remove succeeded, want error")
	}
	names, _ = s.Names()
	if !slices.Equal(names, []string{"lite"}) {
		t.Errorf("Names after remove = %v, want [lite]", names)
	}
}

func TestFileStorePermissions(t *testing.T) {
	dir := t.TempDir()
	s := &fileStore{dir: dir}
	if err := s.Add("pro", "sk-1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("keys.json mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSecurityArgs(t *testing.T) {
	tests := []struct {
		op   string
		want []string
	}{
		{"add", []string{"add-generic-password", "-s", "ferry", "-a", "pro", "-w", "sk-1", "-U"}},
		{"find", []string{"find-generic-password", "-s", "ferry", "-a", "pro", "-w"}},
		{"delete", []string{"delete-generic-password", "-s", "ferry", "-a", "pro"}},
	}
	for _, tt := range tests {
		got := securityArgs(tt.op, "ferry", "pro", "sk-1")
		if !slices.Equal(got, tt.want) {
			t.Errorf("securityArgs(%q) = %v, want %v", tt.op, got, tt.want)
		}
	}
}
