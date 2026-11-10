package wb_history

import "github.com/eclipse/paho.mqtt.golang/packets"

// discardStore исключает повтор RPC после переподключения.
type discardStore struct{}

// Open открывает пустое хранилище.
func (discardStore) Open() {}

// Put отбрасывает пакет.
func (discardStore) Put(string, packets.ControlPacket) {}

// Get возвращает отсутствие сохранённого пакета.
func (discardStore) Get(string) packets.ControlPacket { return nil }

// All возвращает пустой список пакетов.
func (discardStore) All() []string { return nil }

// Del удаляет пакет из пустого хранилища.
func (discardStore) Del(string) {}

// Close закрывает пустое хранилище.
func (discardStore) Close() {}

// Reset очищает пустое хранилище.
func (discardStore) Reset() {}
