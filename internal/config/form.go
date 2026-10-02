package config

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	// TokenStatusSet — значение поля статуса, когда токен задан.
	TokenStatusSet = "set"
	// TokenStatusUnset — значение поля статуса, когда токена нет.
	TokenStatusUnset = "unset"
)

// tokenPattern — формат токена Telegram-бота.
var tokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// form — конфиг в том виде, в котором его видит форма UI.
type form struct {
	Config
	Token       string `json:"token"`
	DeleteToken bool   `json:"delete_token"`
	TokenStatus string `json:"token_status"`
}

// ToJSON готовит конфиг для формы: правила не проверяются, токен не читается и не отдаётся.
func ToJSON(configData []byte, tokens *TokenStore) ([]byte, error) {
	cfg, errDecode := decode(configData)
	if errDecode != nil {
		return nil, errDecode
	}

	exists, errExists := tokens.Exists()
	if errExists != nil {
		return nil, errExists
	}

	status := TokenStatusUnset
	if exists {
		status = TokenStatusSet
	}

	out, errMarshal := json.MarshalIndent(form{Config: *cfg, TokenStatus: status}, "", "    ")
	if errMarshal != nil {
		return nil, &Error{kind: kindInvalid, Op: "encode", Message: errMarshal.Error()}
	}

	return append(out, '\n'), nil
}

// FromJSON проверяет ввод формы, применяет действие с токеном последним шагом
// и возвращает конфиг без токена для записи на диск. Поле token_status из формы
// принимается и отбрасывается.
func FromJSON(formData []byte, tokens *TokenStore) ([]byte, error) {
	var in form

	errUnmarshal := json.Unmarshal(formData, &in)
	if errUnmarshal != nil {
		return nil, &Error{kind: kindInvalid, Op: "decode", Message: errUnmarshal.Error()}
	}

	cfg := in.Config
	cfg.applyDefaults()

	errValidate := cfg.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	newToken := strings.TrimSpace(in.Token)

	if newToken != "" && in.DeleteToken {
		return nil, &Error{kind: kindInvalid, Op: "token", Message: "нельзя одновременно задать новый токен и удалить текущий"}
	}

	if newToken != "" && !tokenPattern.MatchString(newToken) {
		return nil, &Error{kind: kindInvalid, Op: "token", Message: "токен не похож на токен Telegram-бота (ожидается 123456:ABC...)"}
	}

	out, errMarshal := json.MarshalIndent(cfg, "", "    ")
	if errMarshal != nil {
		return nil, &Error{kind: kindInvalid, Op: "encode", Message: errMarshal.Error()}
	}

	errToken := applyTokenAction(tokens, newToken, in.DeleteToken)
	if errToken != nil {
		return nil, errToken
	}

	return append(out, '\n'), nil
}

// applyTokenAction удаляет или перезаписывает токен; без действия файл не трогается.
func applyTokenAction(tokens *TokenStore, newToken string, deleteToken bool) error {
	switch {
	case deleteToken:
		return tokens.Delete()
	case newToken != "":
		return tokens.Write(newToken)
	default:
		return nil
	}
}
