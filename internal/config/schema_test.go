package config

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	// schemaPath — путь к схеме confed относительно пакета.
	schemaPath = "../../wb/confed/wb-telegram.schema.json"
	// defaultConfigPath — путь к конфигу по умолчанию из пакета относительно пакета.
	defaultConfigPath = "../../wb/default-config/wb-telegram.conf"
)

// loadSchema читает и разбирает схему confed.
func loadSchema(t *testing.T) map[string]any {
	t.Helper()

	data, errRead := os.ReadFile(schemaPath)
	if errRead != nil {
		t.Fatalf("read schema: %v", errRead)
	}

	var schema map[string]any

	errUnmarshal := json.Unmarshal(data, &schema)
	if errUnmarshal != nil {
		t.Fatalf("schema is not JSON: %v", errUnmarshal)
	}

	return schema
}

// jsonFields возвращает имена JSON-полей структуры, включая встроенные.
func jsonFields(typ reflect.Type) []string {
	var out []string

	for field := range typ.Fields() {
		if field.Anonymous {
			out = append(out, jsonFields(field.Type)...)

			continue
		}

		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		out = append(out, name)
	}

	slices.Sort(out)

	return out
}

// keys возвращает отсортированные ключи объекта.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}

	slices.Sort(out)

	return out
}

// lookup спускается по вложенным объектам схемы и возвращает значение по пути.
func lookup(node any, path ...string) any {
	for _, key := range path {
		object, _ := node.(map[string]any)
		node = object[key]
	}

	return node
}

// TestSchemaConfigFile проверяет блок configFile схемы.
func TestSchemaConfigFile(t *testing.T) {
	t.Parallel()

	configFile, _ := loadSchema(t)["configFile"].(map[string]any)

	want := map[string]any{
		"path":     DefaultConfigPath,
		"service":  "wb-telegram",
		"toJSON":   []any{"/usr/bin/wb-telegram", "-to-json"},
		"fromJSON": []any{"/usr/bin/wb-telegram", "-from-json"},
	}

	if !reflect.DeepEqual(configFile, want) {
		t.Fatalf("configFile = %v, want %v", configFile, want)
	}
}

// TestSchemaMatchesStructs проверяет, что поля формы в схеме совпадают с Go-структурами.
func TestSchemaMatchesStructs(t *testing.T) {
	t.Parallel()

	properties, _ := loadSchema(t)["properties"].(map[string]any)

	tests := []struct {
		name string
		path []string
		typ  reflect.Type
	}{
		{name: "form", typ: reflect.TypeFor[form]()},
		{name: "user", path: []string{"users", "items", "properties"}, typ: reflect.TypeFor[User]()},
		{name: "command", path: []string{"commands", "items", "properties"}, typ: reflect.TypeFor[Command]()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			props, _ := lookup(properties, tt.path...).(map[string]any)

			if got, want := keys(props), jsonFields(tt.typ); !slices.Equal(got, want) {
				t.Fatalf("properties = %v, want %v", got, want)
			}
		})
	}
}

// TestSchemaLimits проверяет, что ограничения схемы совпадают с проверками Validate.
func TestSchemaLimits(t *testing.T) {
	t.Parallel()

	properties, _ := loadSchema(t)["properties"].(map[string]any)

	reserved := make([]any, 0, len(reservedCommands))
	for _, name := range reservedCommands {
		reserved = append(reserved, name)
	}

	tests := []struct {
		name string
		path []string
		want any
	}{
		{name: "commands max items", path: []string{"commands", "maxItems"}, want: float64(MaxCommands)},
		{name: "timeout minimum", path: []string{"reply_timeout_s", "minimum"}, want: float64(1)},
		{name: "timeout maximum", path: []string{"reply_timeout_s", "maximum"}, want: float64(MaxReplyTimeoutS)},
		{name: "timeout default", path: []string{"reply_timeout_s", "default"}, want: float64(DefaultReplyTimeoutS)},
		{name: "token pattern", path: []string{"token", "pattern"}, want: "^$|" + tokenPattern.String()},
		{name: "token status read-only", path: []string{"token_status", "readonly"}, want: true},
		{name: "token status values", path: []string{"token_status", "enum"}, want: []any{TokenStatusSet, TokenStatusUnset}},
		{name: "user name min length", path: []string{"users", "items", "properties", "name", "minLength"}, want: float64(1)},
		{name: "command name pattern", path: []string{"commands", "items", "properties", "name", "pattern"}, want: commandNamePattern.String()},
		{name: "reserved commands", path: []string{"commands", "items", "properties", "name", "not", "enum"}, want: reserved},
		{name: "description min length", path: []string{"commands", "items", "properties", "description", "minLength"}, want: float64(1)},
		{name: "description max length", path: []string{"commands", "items", "properties", "description", "maxLength"}, want: float64(maxDescriptionRunes)},
		{name: "allowed users unique", path: []string{"commands", "items", "properties", "users", "uniqueItems"}, want: true},
		{name: "allowed users source", path: []string{"commands", "items", "properties", "users", "items", "watch", "users"}, want: "root.users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := lookup(properties, tt.path...); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("%s = %v, want %v", strings.Join(tt.path, "."), got, tt.want)
			}
		})
	}
}

// uiStrings собирает тексты UI схемы: title, description, enum_titles и patternmessage.
// Шаблоны JSON Editor ({{item.name}}) не переводятся и пропускаются.
func uiStrings(node any) []string {
	var out []string

	add := func(text string) {
		if !strings.Contains(text, "{{") {
			out = append(out, text)
		}
	}

	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "translations" {
				continue
			}

			if text, ok := child.(string); ok && (key == "title" || key == "description" || key == "patternmessage") {
				add(text)
			}

			if titles, ok := child.([]any); ok && key == "enum_titles" {
				for _, title := range titles {
					text, _ := title.(string)
					add(text)
				}
			}

			out = append(out, uiStrings(child)...)
		}
	case []any:
		for _, child := range value {
			out = append(out, uiStrings(child)...)
		}
	}

	return out
}

// TestSchemaTranslations проверяет, что у каждого текста UI есть русский перевод,
// а у каждого ключа-описания — английский.
func TestSchemaTranslations(t *testing.T) {
	t.Parallel()

	schema := loadSchema(t)
	en, _ := lookup(schema, "translations", "en").(map[string]any)
	ru, _ := lookup(schema, "translations", "ru").(map[string]any)

	for _, text := range uiStrings(schema) {
		if _, translated := ru[text]; !translated {
			t.Errorf("no ru translation for %q", text)
		}

		if _, translated := en[text]; strings.HasSuffix(text, "_description") && !translated {
			t.Errorf("no en translation for %q", text)
		}
	}

	for key := range en {
		if _, translated := ru[key]; !translated {
			t.Errorf("en key %q has no ru translation", key)
		}
	}
}

// TestDefaultConfigIsValid проверяет конфиг по умолчанию из пакета.
func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	data, errRead := os.ReadFile(defaultConfigPath)
	if errRead != nil {
		t.Fatalf("read default config: %v", errRead)
	}

	cfg, errParse := Parse(data)
	if errParse != nil {
		t.Fatalf("default config: %v", errParse)
	}

	if cfg.Enabled {
		t.Fatal("default config must be disabled")
	}

	if len(cfg.Users) != 0 || len(cfg.Commands) != 0 {
		t.Fatalf("default config must have no users and commands: %+v", cfg)
	}
}
