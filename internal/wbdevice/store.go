package wbdevice

import (
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// discardStore — хранилище paho, которое ничего не хранит.
//
// Устройство при каждом (пере)подключении само публикует всё состояние заново, поэтому
// досылка paho неподтверждённых сообщений после переподключения не нужна и вредна:
// она идёт параллельно с onConnect (гонка данных внутри paho на общем пакете) и может
// прислать старое значение контрола после нового.
//
// Цена: публикация, не подтверждённая брокером к моменту обрыва, не досылается —
// ожидающий её SetValue/SetError завершается ErrTimeout, а её message id остаётся
// занятым в paho до конца жизни клиента (на практике единицы на обрыв из 65535).
type discardStore struct{}

// Open ничего не делает.
func (discardStore) Open() {}

// Put отбрасывает сообщение.
func (discardStore) Put(string, packets.ControlPacket) {}

// Get всегда возвращает nil: сохранённых сообщений нет.
func (discardStore) Get(string) packets.ControlPacket {
	return nil
}

// All всегда возвращает пустой список ключей.
func (discardStore) All() []string {
	return nil
}

// Del ничего не делает.
func (discardStore) Del(string) {}

// Close ничего не делает.
func (discardStore) Close() {}

// Reset ничего не делает.
func (discardStore) Reset() {}
