package upstream

type Message struct {
	Topic   string
	Payload []byte
}

type Client interface {
	Publish(Message) error
}
