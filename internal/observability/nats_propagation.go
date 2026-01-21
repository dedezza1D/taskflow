package observability

import "github.com/nats-io/nats.go"

// NATSHeaderCarrier adapts nats.Header to the OpenTelemetry TextMapCarrier interface.
type NATSHeaderCarrier struct {
	H nats.Header
}

func (c NATSHeaderCarrier) Get(key string) string {
	return c.H.Get(key)
}

func (c NATSHeaderCarrier) Set(key string, value string) {
	c.H.Set(key, value)
}

func (c NATSHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c.H))
	for k := range c.H {
		keys = append(keys, k)
	}
	return keys
}
