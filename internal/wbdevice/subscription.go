package wbdevice

import paho "github.com/eclipse/paho.mqtt.golang"

//go:generate ../../bin/mockgen -source=subscription.go -destination=mock/subscription_mock.go -package=mock -mock_names=subscriptionToken=MockSubscriptionToken

// subscriptionToken содержит подтверждение подписки MQTT.
type subscriptionToken interface {
	paho.Token
	Result() map[string]byte
}
