// Package main — точка входа сервиса wb-telegram.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/Format-C-eft/wb-telegram/internal/config"
)

// version задаётся при сборке через -ldflags "-X main.version=...".
var version = "dev"

// main передаёт управление run и завершает процесс с её кодом.
func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run разбирает флаги, выполняет выбранный режим и возвращает код выхода.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wb-telegram", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", config.DefaultConfigPath, "path to the config written by wb-mqtt-confed")
	tokenPath := flags.String("token-file", config.DefaultTokenPath, "path to the bot token file")
	broker := flags.String("broker", "", "MQTT broker URL (default: mosquitto unix socket or tcp://localhost:1883)")
	debug := flags.Bool("debug", false, "enable debug logging")
	toJSON := flags.Bool("to-json", false, "confed hook: read the config on stdin, print it for the web UI and exit")
	fromJSON := flags.Bool("from-json", false, "confed hook: read the config from the web UI on stdin, print it for disk and exit")
	showVersion := flags.Bool("version", false, "print version and exit")

	errParse := flags.Parse(args)
	if errParse != nil {
		return 2
	}

	switch {
	case *showVersion:
		_, errPrint := fmt.Fprintln(stdout, version)
		if errPrint != nil {
			return 1
		}

		return 0
	case *toJSON:
		return runToJSON(*tokenPath, stdin, stdout, stderr)
	case *fromJSON:
		return runFromJSON(*tokenPath, stdin, stdout, stderr)
	}

	logLevel := setupLogger(stdout, *debug)

	return runService(context.Background(), serviceSettings{
		configPath: *configPath,
		tokenPath:  *tokenPath,
		broker:     *broker,
		logLevel:   logLevel,
	})
}

// runToJSON печатает конфиг для формы UI (крючок toJSON wb-mqtt-confed): confed вызывает его
// без аргументов и подаёт содержимое файла конфига на stdin.
func runToJSON(tokenPath string, stdin io.Reader, stdout, stderr io.Writer) int {
	data, errRead := io.ReadAll(stdin)
	if errRead != nil {
		return fail(stderr, errRead)
	}

	out, errTo := config.ToJSON(data, config.NewTokenStore(tokenPath))
	if errTo != nil {
		return fail(stderr, errTo)
	}

	_, errWrite := stdout.Write(out)
	if errWrite != nil {
		return fail(stderr, errWrite)
	}

	return 0
}

// runFromJSON проверяет ввод формы и печатает конфиг для записи на диск (крючок fromJSON wb-mqtt-confed).
func runFromJSON(tokenPath string, stdin io.Reader, stdout, stderr io.Writer) int {
	data, errRead := io.ReadAll(stdin)
	if errRead != nil {
		return fail(stderr, errRead)
	}

	out, errFrom := config.FromJSON(data, config.NewTokenStore(tokenPath))
	if errFrom != nil {
		return fail(stderr, errFrom)
	}

	_, errWrite := stdout.Write(out)
	if errWrite != nil {
		return fail(stderr, errWrite)
	}

	return 0
}

// fail печатает причину отказа в stderr (её показывает журнал wb-mqtt-confed) и возвращает код 1.
func fail(stderr io.Writer, err error) int {
	// Ошибку записи в stderr сообщить больше некуда: код выхода 1 остаётся сигналом отказа в любом случае.
	_, _ = fmt.Fprintln(stderr, err)

	return 1
}

// setupLogger один раз за процесс настраивает глобальный slog: текст в w (journald добавит время),
// уровень — через возвращаемый LevelVar, чтобы сервис мог включить отладку по конфигу.
func setupLogger(w io.Writer, debug bool) *slog.LevelVar {
	level := new(slog.LevelVar)

	if debug {
		level.Set(slog.LevelDebug)
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return attr
		},
	})

	slog.SetDefault(slog.New(handler))

	return level
}
