package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TokenStore хранит токен бота в отдельном файле с правами 0600.
type TokenStore struct {
	path string
}

// NewTokenStore создаёт хранилище токена по пути path.
func NewTokenStore(path string) *TokenStore {
	return &TokenStore{path: path}
}

// Exists сообщает, есть ли файл токена; содержимое не читается.
func (s *TokenStore) Exists() (bool, error) {
	_, errStat := os.Stat(s.path)
	if errors.Is(errStat, fs.ErrNotExist) {
		return false, nil
	}

	if errStat != nil {
		return false, &Error{kind: kindIO, Op: "stat", Path: s.path, Message: errStat.Error()}
	}

	return true, nil
}

// Read возвращает токен; отсутствующий или пустой файл даёт ErrNoToken.
func (s *TokenStore) Read() (string, error) {
	data, errRead := os.ReadFile(s.path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return "", ErrNoToken
	}

	if errRead != nil {
		return "", &Error{kind: kindIO, Op: "read", Path: s.path, Message: errRead.Error()}
	}

	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", ErrNoToken
	}

	return token, nil
}

// Write атомарно перезаписывает токен: временный файл 0600 и rename в том же каталоге.
func (s *TokenStore) Write(token string) error {
	dir := filepath.Dir(s.path)

	errMkdir := os.MkdirAll(dir, 0o700)
	if errMkdir != nil {
		return &Error{kind: kindIO, Op: "mkdir", Path: dir, Message: errMkdir.Error()}
	}

	errChmodDir := os.Chmod(dir, 0o700) //nolint:gosec // у каталога нужен бит x для владельца
	if errChmodDir != nil {
		return &Error{kind: kindIO, Op: "chmod", Path: dir, Message: errChmodDir.Error()}
	}

	tmp, errCreate := os.CreateTemp(dir, ".token-*")
	if errCreate != nil {
		return &Error{kind: kindIO, Op: "create", Path: dir, Message: errCreate.Error()}
	}

	tmpPath := tmp.Name()

	errWrite := writeAndClose(tmp, token)
	if errWrite != nil {
		_ = os.Remove(tmpPath)

		return &Error{kind: kindIO, Op: "write", Path: tmpPath, Message: errWrite.Error()}
	}

	errRename := os.Rename(tmpPath, s.path)
	if errRename != nil {
		_ = os.Remove(tmpPath)

		return &Error{kind: kindIO, Op: "rename", Path: s.path, Message: errRename.Error()}
	}

	return nil
}

// writeAndClose выставляет права 0600, пишет токен, синхронизирует и закрывает файл.
func writeAndClose(f *os.File, token string) error {
	errChmod := f.Chmod(0o600)
	if errChmod != nil {
		_ = f.Close()

		return errChmod
	}

	_, errWrite := f.WriteString(token + "\n")
	if errWrite != nil {
		_ = f.Close()

		return errWrite
	}

	errSync := f.Sync()
	if errSync != nil {
		_ = f.Close()

		return errSync
	}

	return f.Close()
}

// Delete удаляет файл токена; отсутствие файла ошибкой не считается.
func (s *TokenStore) Delete() error {
	errRemove := os.Remove(s.path)
	if errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
		return &Error{kind: kindIO, Op: "remove", Path: s.path, Message: errRemove.Error()}
	}

	return nil
}
