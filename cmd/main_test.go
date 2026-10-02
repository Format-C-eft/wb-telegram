package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain глушит журнал сервиса, чтобы вывод тестов оставался чистым.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	os.Exit(m.Run())
}

// TestRunVersion проверяет режим -version и обработку неизвестного флага.
func TestRunVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{name: "version", args: []string{"-version"}, wantCode: 0, wantOut: "dev"},
		{name: "unknown flag", args: []string{"-nope"}, wantCode: 2, wantOut: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := run(tt.args, strings.NewReader(""), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}

			if strings.TrimSpace(stdout.String()) != tt.wantOut {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
		})
	}
}

// TestRunToJSON проверяет крючок -to-json: конфиг приходит на stdin (так его подаёт wb-mqtt-confed),
// токен не выдаётся, статус токена выдаётся, битый JSON даёт код 1.
func TestRunToJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		stdin       string
		token       string
		wantCode    int
		wantEnabled bool
		wantStatus  string
	}{
		{name: "config and token", stdin: `{"enabled": true}`, token: "1:secret\n", wantCode: 0, wantEnabled: true, wantStatus: "set"},
		{name: "empty stdin, no token", stdin: "", wantCode: 0, wantEnabled: false, wantStatus: "unset"},
		{name: "broken json", stdin: `{"enabled": tru`, token: "1:secret\n", wantCode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tokenPath := filepath.Join(t.TempDir(), "token")

			if tt.token != "" {
				errWrite := os.WriteFile(tokenPath, []byte(tt.token), 0o600)
				if errWrite != nil {
					t.Fatalf("write token: %v", errWrite)
				}
			}

			var stdout, stderr bytes.Buffer

			code := run([]string{"-to-json", "-token-file", tokenPath}, strings.NewReader(tt.stdin), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d, stderr = %s", code, tt.wantCode, stderr.String())
			}

			if strings.Contains(stdout.String(), "secret") || strings.Contains(stderr.String(), "secret") {
				t.Fatalf("token leaked: stdout = %s, stderr = %s", stdout.String(), stderr.String())
			}

			if tt.wantCode != 0 {
				if stdout.Len() != 0 || stderr.Len() == 0 {
					t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
				}

				return
			}

			var got map[string]any

			errUnmarshal := json.Unmarshal(stdout.Bytes(), &got)
			if errUnmarshal != nil || got["token_status"] != tt.wantStatus || got["enabled"] != tt.wantEnabled {
				t.Fatalf("stdout = %s, err = %v", stdout.String(), errUnmarshal)
			}
		})
	}
}

// TestRunFromJSON проверяет крючок -from-json: успешная запись токена и отказ на ошибке.
func TestRunFromJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantCode  int
		wantToken string
		wantErr   string
	}{
		{
			name:      "ok",
			input:     `{"enabled": true, "users": [{"name": "Я", "chat_id": 1}], "commands": [], "token": "1:new"}`,
			wantCode:  0,
			wantToken: "1:new\n",
		},
		{
			name:     "duplicate users",
			input:    `{"users": [{"name": "Я", "chat_id": 1}, {"name": "Я", "chat_id": 2}], "token": "1:new"}`,
			wantCode: 1,
			wantErr:  "повторяется имя пользователя",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tokenPath := filepath.Join(t.TempDir(), "wb-telegram", "token")

			var stdout, stderr bytes.Buffer

			code := run([]string{"-from-json", "-token-file", tokenPath}, strings.NewReader(tt.input), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d, stderr = %s", code, tt.wantCode, stderr.String())
			}

			if tt.wantErr != "" {
				if !strings.Contains(stderr.String(), tt.wantErr) || stdout.Len() != 0 {
					t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
				}

				return
			}

			token, _ := os.ReadFile(tokenPath) //nolint:gosec // путь из t.TempDir()
			if string(token) != tt.wantToken || strings.Contains(stdout.String(), "token") {
				t.Fatalf("token = %q, stdout = %s", token, stdout.String())
			}
		})
	}
}
