package wb_history

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history/mock"
)

type historyFixture struct {
	client  *Client
	mqtt    *mock.MockClient
	ctrl    *gomock.Controller
	config  *paho.ClientOptions
	handler paho.MessageHandler
}

// newHistoryFixture создаёт клиент с моками MQTT без подключения к сети.
func newHistoryFixture(t *testing.T, opts ...Option) *historyFixture {
	t.Helper()

	fixture := &historyFixture{ctrl: gomock.NewController(t)}
	fixture.mqtt = mock.NewMockClient(fixture.ctrl)
	factory := func(o *options) {
		o.clientFactory = func(config *paho.ClientOptions) paho.Client {
			fixture.config = config

			return fixture.mqtt
		}
	}
	opts = append(opts, factory)

	var errNew error

	fixture.client, errNew = New(opts...)
	if errNew != nil {
		t.Fatal(errNew)
	}

	fixture.mqtt.EXPECT().Disconnect(uint(0))
	t.Cleanup(func() { _ = fixture.client.Stop() })

	return fixture
}

// completedToken создаёт подтверждённую операцию MQTT.
func completedToken(ctrl *gomock.Controller, err error) *mock.MockToken {
	token := mock.NewMockToken(ctrl)
	done := make(chan struct{})
	close(done)
	token.EXPECT().Done().Return(done).AnyTimes()
	token.EXPECT().Error().Return(err).AnyTimes()

	return token
}

// subscription создаёт подтверждение SUBACK с заданным кодом.
func (f *historyFixture) subscription(code byte) *mock.MockSubscriptionToken {
	token := mock.NewMockSubscriptionToken(f.ctrl)
	done := make(chan struct{})
	close(done)
	token.EXPECT().Done().Return(done)
	token.EXPECT().Error().Return(nil)
	token.EXPECT().Result().Return(map[string]byte{f.client.topic + "/reply": code})

	return token
}

// expectSubscribe задаёт операцию подписки и сохраняет обработчик ответа.
func (f *historyFixture) expectSubscribe(token paho.Token) *gomock.Call {
	return f.mqtt.EXPECT().Subscribe(f.client.topic+"/reply", byte(1), gomock.Any()).DoAndReturn(func(_ string, _ byte, handler paho.MessageHandler) paho.Token {
		f.handler = handler

		return token
	})
}

// run запускает worker и моделирует подтверждение подключения и подписки.
func (f *historyFixture) run(t *testing.T) {
	t.Helper()

	f.mqtt.EXPECT().Connect().Return(completedToken(f.ctrl, nil))
	f.mqtt.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	f.expectSubscribe(f.subscription(1))

	errRun := f.client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	f.config.OnConnect(f.mqtt)

	if !f.client.Ready() {
		t.Fatal("history not ready after SUBACK")
	}
}

// publish ожидает единственный JSON-запрос и передаёт его тесту.
func (f *historyFixture) publish(t *testing.T, token paho.Token) <-chan request {
	t.Helper()

	requests := make(chan request, 1)

	f.mqtt.EXPECT().Publish(f.client.topic, byte(1), false, gomock.Any()).DoAndReturn(func(_ string, _ byte, _ bool, payload any) paho.Token {
		wire, ok := payload.([]byte)
		if !ok {
			t.Error("request is not JSON bytes")

			return token
		}

		var query request

		errJSON := json.Unmarshal(wire, &query)
		if errJSON != nil {
			t.Error(errJSON)
		}

		requests <- query

		return token
	})

	return requests
}

// deliver передаёт MQTT-сообщение подписанному обработчику.
func (f *historyFixture) deliver(payload string, retained bool) {
	message := mock.NewMockMessage(f.ctrl)
	message.EXPECT().Retained().Return(retained)

	if !retained {
		message.EXPECT().Payload().Return([]byte(payload)).AnyTimes()
	}

	f.handler(f.mqtt, message)
}

// TestReadRPC проверяет wire-запрос и ответ до PUBACK.
func TestReadRPC(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	puback := mock.NewMockToken(fixture.ctrl)
	puback.EXPECT().Done().Return(make(chan struct{}))
	requests := fixture.publish(t, puback)
	result := make(chan response, 1)

	go func() {
		points, errRead := fixture.client.Read(context.Background(), historyQuery())
		result <- response{points: points, err: errRead}
	}()

	query := <-requests
	if !validRequest(query) {
		t.Fatalf("query=%+v", query)
	}

	fixture.deliver(`{"id":"`+query.ID+`","result":{"values":[{"c":42,"i":1,"t":1794268801.125,"v":"22.5"}],"has_more":false}}`, false)

	select {
	case received := <-result:
		if received.err != nil || len(received.points) != 1 || received.points[0].Value != 22.5 {
			t.Fatalf("response=%+v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("reply waited for PUBACK")
	}
}

// TestLifecycle проверяет недоступность, занятость и отключение клиента.
func TestLifecycle(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)

	_, errBefore := fixture.client.Read(context.Background(), historyQuery())
	if !errors.Is(errBefore, ErrUnavailable) {
		t.Fatal(errBefore)
	}

	fixture.run(t)
	requests := fixture.publish(t, completedToken(fixture.ctrl, nil))
	result := make(chan error, 1)

	go func() { _, errRead := fixture.client.Read(context.Background(), historyQuery()); result <- errRead }()

	<-requests

	_, errBusy := fixture.client.Read(context.Background(), historyQuery())
	if !errors.Is(errBusy, ErrBusy) {
		t.Fatal(errBusy)
	}

	fixture.config.OnReconnecting(fixture.mqtt, fixture.config)

	if !errors.Is(<-result, ErrUnavailable) || fixture.client.Ready() {
		t.Fatal("disconnect did not cancel pending or reset ready")
	}

	errStop := fixture.client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}

	_, errClosed := fixture.client.Read(context.Background(), historyQuery())
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
