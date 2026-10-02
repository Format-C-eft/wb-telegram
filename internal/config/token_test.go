package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTokenStoreLifecycle проверяет запись, чтение, права и удаление токена.
func TestTokenStoreLifecycle(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "wb-telegram")
	store := NewTokenStore(filepath.Join(dir, "token"))

	_, errRead := store.Read()
	if !errors.Is(errRead, ErrNoToken) {
		t.Fatalf("Read on missing file = %v, want ErrNoToken", errRead)
	}

	errWrite := store.Write("123:abc")
	if errWrite != nil {
		t.Fatalf("Write: %v", errWrite)
	}

	info, errStat := os.Stat(filepath.Join(dir, "token"))
	if errStat != nil {
		t.Fatalf("Stat: %v", errStat)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token perm = %v, want 0600", info.Mode().Perm())
	}

	dirInfo, errDirStat := os.Stat(dir)
	if errDirStat != nil {
		t.Fatalf("Stat dir: %v", errDirStat)
	}

	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %v, want 0700", dirInfo.Mode().Perm())
	}

	token, errReadBack := store.Read()
	if errReadBack != nil || token != "123:abc" {
		t.Fatalf("Read = %q, %v", token, errReadBack)
	}

	exists, errExists := store.Exists()
	if errExists != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, errExists)
	}

	errDelete := store.Delete()
	if errDelete != nil {
		t.Fatalf("Delete: %v", errDelete)
	}

	errDeleteAgain := store.Delete()
	if errDeleteAgain != nil {
		t.Fatalf("Delete of missing token: %v", errDeleteAgain)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

// TestTokenStoreEmptyFile проверяет, что пустой файл токена считается отсутствующим токеном.
func TestTokenStoreEmptyFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")

	errWrite := os.WriteFile(path, []byte(" \n"), 0o600)
	if errWrite != nil {
		t.Fatalf("WriteFile: %v", errWrite)
	}

	_, errRead := NewTokenStore(path).Read()
	if !errors.Is(errRead, ErrNoToken) {
		t.Fatalf("Read = %v, want ErrNoToken", errRead)
	}
}
