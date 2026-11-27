package wbdevice

import (
	"context"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// testControls возвращает записываемый контрол и команду с временным значением.
func testControls() []Control {
	return []Control{
		{ID: "send", Meta: ControlMeta{Type: ControlTypeText, Title: map[string]string{"en": "Message", "ru": "Сообщение"}, Order: 1}},
		{ID: "cmd_gate", Meta: ControlMeta{Type: ControlTypeText, Title: map[string]string{"en": "/gate", "ru": "Ворота"}, Order: 2, Readonly: true}, Initial: "{}", Volatile: true},
	}
}

// newMockDevice создаёт устройство с изолированным MQTT-клиентом.
func newMockDevice(t *testing.T, opts ...Option) (*Device, *mock.MockClient, *paho.ClientOptions, *gomock.Controller) {
	t.Helper()

	controller := gomock.NewController(t)
	client := mock.NewMockClient(controller)

	var configured *paho.ClientOptions

	factory := func(o *options) {
		o.clientFactory = func(clientOptions *paho.ClientOptions) paho.Client {
			configured = clientOptions

			return client
		}
	}
	all := []Option{WithControls(testControls()...), WithStaleWindow(time.Nanosecond), WithTimeout(50 * time.Millisecond), factory}
	all = append(all, opts...)

	dev, errNew := New("telegram_bot", all...)
	if errNew != nil {
		t.Fatal(errNew)
	}

	t.Cleanup(func() {
		dev.stopOnce.Do(func() { close(dev.stop) })
		dev.workers.Wait()
	})

	return dev, client, configured, controller
}

// completedToken возвращает подтверждённый токен операции.
func completedToken(controller *gomock.Controller) *mock.MockToken {
	token := mock.NewMockToken(controller)
	done := make(chan struct{})
	close(done)
	token.EXPECT().Done().Return(done).AnyTimes()
	token.EXPECT().WaitTimeout(gomock.Any()).Return(true).AnyTimes()
	token.EXPECT().Error().Return(nil).AnyTimes()

	return token
}

// subscribedToken возвращает результат SUBACK для фильтра.
func subscribedToken(controller *gomock.Controller, topic string, qos byte) *mock.MockSubscriptionToken {
	token := mock.NewMockSubscriptionToken(controller)
	token.EXPECT().WaitTimeout(gomock.Any()).Return(true)
	token.EXPECT().Error().Return(nil)
	token.EXPECT().Result().Return(map[string]byte{topic: qos})

	return token
}

// expectPublish задаёт публикацию retained-сообщения с QoS 1.
func expectPublish(client *mock.MockClient, controller *gomock.Controller, topic, payload string) *gomock.Call {
	return client.EXPECT().Publish(topic, byte(1), true, payload).Return(completedToken(controller))
}

// expectCollection задаёт сбор retained-топиков без сетевого подключения.
func expectCollection(client *mock.MockClient, controller *gomock.Controller, filter string) {
	client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).Return(subscribedToken(controller, filter, 1))
	client.EXPECT().Unsubscribe(filter).Return(completedToken(controller))
}

// expectState задаёт публикацию метаданных и текущего состояния устройства.
func expectState(client *mock.MockClient, controller *gomock.Controller, send, command, flags string) {
	expectPublish(client, controller, deviceMetaTopic("telegram_bot"), `{"driver":"wb-telegram","title":{"en":"Telegram Bot","ru":"Telegram-бот"}}`)
	expectPublish(client, controller, controlMetaTopic("telegram_bot", "send"), `{"type":"text","title":{"en":"Message","ru":"Сообщение"},"order":1}`)

	if send != "" {
		expectPublish(client, controller, controlTopic("telegram_bot", "send"), send)
	}

	expectPublish(client, controller, controlErrorTopic("telegram_bot", "send"), flags)
	expectPublish(client, controller, controlMetaTopic("telegram_bot", "cmd_gate"), `{"type":"text","title":{"en":"/gate","ru":"Ворота"},"order":2,"readonly":true}`)
	expectPublish(client, controller, controlTopic("telegram_bot", "cmd_gate"), command)
	expectPublish(client, controller, controlErrorTopic("telegram_bot", "cmd_gate"), "")
}

// expectWrites задаёт подписку на записываемый контрол.
func expectWrites(client *mock.MockClient, controller *gomock.Controller) {
	topic := controlOnTopic("telegram_bot", "send")
	client.EXPECT().Subscribe(topic, byte(1), gomock.Any()).Return(subscribedToken(controller, topic, 1))
}

// mockMessage создаёт MQTT-сообщение с ожидаемыми обращениями к данным.
func mockMessage(controller *gomock.Controller, topic string, payload []byte, retained bool) *mock.MockMessage {
	message := mock.NewMockMessage(controller)
	message.EXPECT().Retained().Return(retained).AnyTimes()
	message.EXPECT().Topic().Return(topic).AnyTimes()
	message.EXPECT().Payload().Return(payload).AnyTimes()

	return message
}

// runDevice запускает доставку и подтверждает завершение подключения.
func runDevice(t *testing.T, dev *Device, client *mock.MockClient, controller *gomock.Controller) {
	t.Helper()

	client.EXPECT().Connect().Return(completedToken(controller))

	errRun := dev.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}
}
