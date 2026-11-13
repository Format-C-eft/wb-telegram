package wb_history

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// localBrokerServer создаёт изолированный MQTT-брокер.
func localBrokerServer(t *testing.T) (*mqtt.Server, string) {
	t.Helper()

	server := mqtt.New(&mqtt.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	errAuth := server.AddHook(new(auth.AllowHook), nil)
	if errAuth != nil {
		t.Fatal(errAuth)
	}

	listener := listeners.NewTCP(listeners.Config{ID: "history", Address: "127.0.0.1:0"})

	errListener := server.AddListener(listener)
	if errListener != nil {
		t.Fatal(errListener)
	}

	errServe := server.Serve()
	if errServe != nil {
		t.Fatal(errServe)
	}

	t.Cleanup(func() {
		deadline := time.Now().Add(time.Second)
		for server.Clients.Len() > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}

		_ = server.Close()
	})

	return server, "tcp://" + listener.Address()
}

// awaitReady ждёт подтверждения подписки клиента.
func awaitReady(t *testing.T, client *Client) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for !client.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if !client.Ready() {
		t.Fatal("history not ready")
	}
}

// TestReadRPC проверяет wire-запрос и ответ быстрее завершения публикации.
func TestReadRPC(t *testing.T) {
	t.Parallel()

	broker := localBroker(t)

	client, errNew := New(WithBroker(broker))
	if errNew != nil {
		t.Fatal(errNew)
	}

	t.Cleanup(func() { _ = client.Stop() })

	requests := make(chan request, 2)
	service := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID("history-service"))

	token := service.Connect()
	if !token.WaitTimeout(time.Second) || token.Error() != nil {
		t.Fatal("service connect")
	}

	t.Cleanup(func() { service.Disconnect(0) })

	subscription := service.Subscribe("/rpc/v1/db_logger/history/get_values/+", 1, func(_ paho.Client, message paho.Message) {
		if message.Retained() {
			t.Error("retained request")
		}

		var query request

		errJSON := json.Unmarshal(message.Payload(), &query)
		if errJSON != nil {
			t.Error(errJSON)

			return
		}

		requests <- query

		reply := `{"id":"` + query.ID + `","result":{"values":[{"c":42,"i":1,"t":1794268801.125,"v":"22.5"}],"has_more":false}}`
		service.Publish(message.Topic()+"/reply", 1, false, reply)
	})
	if !subscription.WaitTimeout(time.Second) || subscription.Error() != nil {
		t.Fatal("subscribe")
	}

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	awaitReady(t, client)

	points, errRead := client.Read(context.Background(), historyQuery())
	if errRead != nil {
		t.Fatal(errRead)
	}

	if len(points) != 1 || points[0].Value != 22.5 {
		t.Fatalf("points=%+v", points)
	}

	select {
	case query := <-requests:
		if !validRequest(query) {
			t.Fatalf("query=%+v", query)
		}
	case <-time.After(time.Second):
		t.Fatal("missing request")
	}
}

// TestLifecycle проверяет недоступность, отмену и завершение клиента.
func TestLifecycle(t *testing.T) {
	t.Parallel()

	client, errNew := New(WithBroker(localBroker(t)))
	if errNew != nil {
		t.Fatal(errNew)
	}

	_, errBefore := client.Read(context.Background(), historyQuery())
	if !errors.Is(errBefore, ErrUnavailable) {
		t.Fatal(errBefore)
	}

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	awaitReady(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()

	result := make(chan error, 1)

	go func() { _, errRead := client.Read(ctx, historyQuery()); result <- errRead }()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		active := client.pending != nil
		client.mu.Unlock()

		if active {
			break
		}

		time.Sleep(time.Millisecond)
	}

	_, errBusy := client.Read(context.Background(), historyQuery())
	if !errors.Is(errBusy, ErrBusy) {
		t.Fatal(errBusy)
	}

	client.onReconnecting(nil, nil)

	if !errors.Is(<-result, ErrUnavailable) {
		t.Fatal("disconnect did not cancel pending")
	}

	if client.Ready() {
		t.Fatal("ready after disconnect")
	}

	errStop := client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}

	_, errClosed := client.Read(context.Background(), historyQuery())
	if !errors.Is(errClosed, ErrClosed) {
		t.Fatal(errClosed)
	}
}

// TestLimits проверяет ограничения декодера до накопления точек.
func TestLimits(t *testing.T) {
	t.Parallel()

	_, errBytes := decode([]byte(strings.Repeat(" ", maxResponseBytes+1)), "1", historyQuery())
	if !errors.Is(errBytes, ErrTooManyData) {
		t.Fatal(errBytes)
	}

	values := strings.Repeat(`{"c":1,"i":1,"t":1794268801,"v":"1"},`, 1001)
	payload := `{"id":"1","result":{"values":[` + strings.TrimSuffix(values, ",") + `],"has_more":false}}`

	_, errCount := decode([]byte(payload), "1", historyQuery())
	if !errors.Is(errCount, ErrTooManyData) {
		t.Fatal(errCount)
	}
}

// validRequest проверяет закреплённые параметры MQTT RPC.
func validRequest(query request) bool {
	return query.ID == "1" && query.Params.Version == 1 && query.Params.MaxRecords == 800 && query.Params.Limit == 1001 && query.Params.Timeout <= 8 && query.Params.Timeout >= 1 && query.Params.Milliseconds && query.Params.Timestamp.GT == 1794268800 && query.Params.Timestamp.LT == 1794355200 && len(query.Params.Channels) == 1 && query.Params.Channels[0] == [2]string{"boiler", "temperature"}
}

// localBroker возвращает адрес изолированного MQTT-брокера.
func localBroker(t *testing.T) string {
	t.Helper()

	_, address := localBrokerServer(t)

	return address
}
