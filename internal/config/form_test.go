package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// formInput — JSON формы с конфигом из одного пользователя и одной команды;
// token_status приходит из формы, как это делает UI.
func formInput(token string, deleteToken bool) []byte {
	in := map[string]any{
		"enabled":         true,
		"reply_timeout_s": 15,
		"debug":           false,
		"users":           []map[string]any{{"name": "Я", "chat_id": 1, "notifications": true}},
		"commands":        []map[string]any{{"name": "gate", "description": "Ворота", "users": []string{}}},
		"token":           token,
		"delete_token":    deleteToken,
		"token_status":    "set",
	}

	data, _ := json.Marshal(in)

	return data
}

// newStoreWithToken создаёт хранилище токена, при необходимости с уже записанным токеном.
func newStoreWithToken(t *testing.T, token string) *TokenStore {
	t.Helper()

	store := NewTokenStore(filepath.Join(t.TempDir(), "wb-telegram", "token"))

	if token != "" {
		errWrite := store.Write(token)
		if errWrite != nil {
			t.Fatalf("Write: %v", errWrite)
		}
	}

	return store
}

// TestToJSONNeverExposesToken проверяет, что токен не попадает в вывод для UI.
func TestToJSONNeverExposesToken(t *testing.T) {
	t.Parallel()

	store := newStoreWithToken(t, "123:supersecret")
	cfg := []byte(`{"enabled": true, "users": [], "commands": []}`)

	out, errTo := ToJSON(cfg, store)
	if errTo != nil {
		t.Fatalf("ToJSON: %v", errTo)
	}

	if bytes.Contains(out, []byte("supersecret")) {
		t.Fatalf("token leaked into UI: %s", out)
	}

	var got map[string]any

	errUnmarshal := json.Unmarshal(out, &got)
	if errUnmarshal != nil {
		t.Fatalf("output is not JSON: %v", errUnmarshal)
	}

	if got["token"] != "" || got["delete_token"] != false || got["enabled"] != true {
		t.Fatalf("unexpected form: %v", got)
	}
}

// TestToJSONStatus проверяет статус токена в форме.
func TestToJSONStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "set", token: "1:a", want: TokenStatusSet},
		{name: "unset", token: "", want: TokenStatusUnset},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, errTo := ToJSON(nil, newStoreWithToken(t, tt.token))
			if errTo != nil {
				t.Fatalf("ToJSON: %v", errTo)
			}

			var got map[string]any

			_ = json.Unmarshal(out, &got)

			if got["token_status"] != tt.want {
				t.Fatalf("token_status = %v, want %s", got["token_status"], tt.want)
			}
		})
	}
}

// TestToJSONToleratesBrokenRules проверяет, что форма открывается и для конфига, нарушающего правила.
func TestToJSONToleratesBrokenRules(t *testing.T) {
	t.Parallel()

	cfg := []byte(`{"enabled": true, "users": [{"name": "", "chat_id": 0}], "commands": []}`)

	_, errTo := ToJSON(cfg, newStoreWithToken(t, ""))
	if errTo != nil {
		t.Fatalf("ToJSON must not validate rules: %v", errTo)
	}
}

// TestFromJSON проверяет действия с токеном и результат для записи на диск.
// Ввод всегда содержит token_status, как при отправке формы из UI.
func TestFromJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		existing    string
		token       string
		deleteToken bool
		wantErr     bool
		wantToken   string
	}{
		{name: "keep existing", existing: "1:old", wantToken: "1:old"},
		{name: "replace", existing: "1:old", token: "2:new", wantToken: "2:new"},
		{name: "set first", token: "2:new", wantToken: "2:new"},
		{name: "delete", existing: "1:old", deleteToken: true, wantToken: ""},
		{name: "token and delete together", existing: "1:old", token: "2:new", deleteToken: true, wantErr: true, wantToken: "1:old"},
		{name: "bad token format", existing: "1:old", token: "not a token", wantErr: true, wantToken: "1:old"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newStoreWithToken(t, tt.existing)

			out, errFrom := FromJSON(formInput(tt.token, tt.deleteToken), store)
			if tt.wantErr {
				if !errors.Is(errFrom, ErrInvalid) {
					t.Fatalf("FromJSON error = %v, want ErrInvalid", errFrom)
				}
			} else {
				if errFrom != nil {
					t.Fatalf("FromJSON: %v", errFrom)
				}

				for _, forbidden := range []string{`"token"`, `"delete_token"`, `"token_status"`} {
					if bytes.Contains(out, []byte(forbidden)) {
						t.Fatalf("output contains %s: %s", forbidden, out)
					}
				}

				cfg, errParse := Parse(out)
				if errParse != nil || cfg.ReplyTimeoutS != 15 {
					t.Fatalf("output is not a valid config: %v, %s", errParse, out)
				}
			}

			token, errRead := store.Read()
			if tt.wantToken == "" {
				if !errors.Is(errRead, ErrNoToken) {
					t.Fatalf("token must be absent, got %q, %v", token, errRead)
				}

				return
			}

			if token != tt.wantToken {
				t.Fatalf("token = %q, want %q", token, tt.wantToken)
			}
		})
	}
}

// TestFromJSONInvalidConfigKeepsToken проверяет, что при ошибке конфига токен не меняется.
func TestFromJSONInvalidConfigKeepsToken(t *testing.T) {
	t.Parallel()

	store := newStoreWithToken(t, "1:old")
	in := []byte(`{"enabled": true, "users": [{"name": "Я", "chat_id": 1}, {"name": "Я", "chat_id": 2}], "commands": [], "token": "2:new"}`)

	_, errFrom := FromJSON(in, store)
	if !errors.Is(errFrom, ErrInvalid) {
		t.Fatalf("FromJSON error = %v, want ErrInvalid", errFrom)
	}

	token, _ := store.Read()
	if token != "1:old" {
		t.Fatalf("token changed to %q", token)
	}
}

// TestFromJSONOutputIsFile проверяет, что вывод загружается как файл конфига и заканчивается переводом строки.
func TestFromJSONOutputIsFile(t *testing.T) {
	t.Parallel()

	out, errFrom := FromJSON(formInput("", false), newStoreWithToken(t, ""))
	if errFrom != nil {
		t.Fatalf("FromJSON: %v", errFrom)
	}

	path := filepath.Join(t.TempDir(), "wb-telegram.conf")

	errWrite := os.WriteFile(path, out, 0o600)
	if errWrite != nil {
		t.Fatalf("WriteFile: %v", errWrite)
	}

	_, errLoad := Load(path)
	if errLoad != nil {
		t.Fatalf("Load written config: %v", errLoad)
	}

	if out[len(out)-1] != '\n' {
		t.Fatal("output must end with newline")
	}
}
