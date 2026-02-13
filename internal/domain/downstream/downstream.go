package downstream

type Frame struct {
	Address byte
	Command byte
	Payload []byte
}

type Client interface {
	Read() (Frame, error)
	Write(Frame) error
}
