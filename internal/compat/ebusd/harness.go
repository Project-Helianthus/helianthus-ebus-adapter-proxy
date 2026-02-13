package ebusd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

const (
	defaultDialTimeout = 2 * time.Second
	defaultIOTimeout   = 2 * time.Second
)

type CommandCase struct {
	Name    string
	Command southboundenh.ENHCommand
	Data    byte
}

type CommandExchange struct {
	Name           string
	Request        [2]byte
	DirectResponse [2]byte
	ProxyResponse  [2]byte
	Match          bool
}

type Result struct {
	AdapterEndpoint string
	ProxyEndpoint   string
	Exchanges       []CommandExchange
}

func (result Result) Compatible() bool {
	if len(result.Exchanges) == 0 {
		return false
	}

	for _, exchange := range result.Exchanges {
		if !exchange.Match {
			return false
		}
	}

	return true
}

func DefaultCommandSet() []CommandCase {
	return []CommandCase{
		{
			Name:    "req_init",
			Command: southboundenh.ENHReqInit,
			Data:    0x11,
		},
		{
			Name:    "req_start",
			Command: southboundenh.ENHReqStart,
			Data:    0x22,
		},
		{
			Name:    "req_info",
			Command: southboundenh.ENHReqInfo,
			Data:    0x33,
		},
		{
			Name:    "req_send",
			Command: southboundenh.ENHReqSend,
			Data:    0x44,
		},
	}
}

func RunConfigOnlyMigrationHarness(ctx context.Context, commandSet []CommandCase) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	selectedCommandSet := normalizedCommandSet(commandSet)
	if len(selectedCommandSet) == 0 {
		return Result{}, errors.New("command set is required")
	}

	adapter, err := startMockAdapter()
	if err != nil {
		return Result{}, err
	}
	defer adapter.Close()

	proxy, err := startCommandProxy(adapter.Address())
	if err != nil {
		return Result{}, err
	}
	defer proxy.Close()

	directResult, err := executeCommandSet(ctx, adapter.Address(), selectedCommandSet)
	if err != nil {
		return Result{}, err
	}

	proxyResult, err := executeCommandSet(ctx, proxy.Address(), selectedCommandSet)
	if err != nil {
		return Result{}, err
	}

	exchanges, err := compareExchanges(directResult, proxyResult)
	if err != nil {
		return Result{}, err
	}

	return Result{
		AdapterEndpoint: adapter.Address(),
		ProxyEndpoint:   proxy.Address(),
		Exchanges:       exchanges,
	}, nil
}

type commandObservation struct {
	name     string
	request  [2]byte
	response [2]byte
}

func executeCommandSet(
	ctx context.Context,
	address string,
	commandSet []CommandCase,
) ([]commandObservation, error) {
	dialer := &net.Dialer{
		Timeout: defaultDialTimeout,
	}

	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial %q: %w", address, err)
	}
	defer connection.Close()

	observations := make([]commandObservation, 0, len(commandSet))

	for _, commandCase := range commandSet {
		if err := maybeSetDeadline(connection); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}

		request := southboundenh.EncodeENH(commandCase.Command, commandCase.Data)
		if _, err := connection.Write(request[:]); err != nil {
			return nil, fmt.Errorf("write %q request: %w", commandCase.Name, err)
		}

		var response [2]byte
		if _, err := io.ReadFull(connection, response[:]); err != nil {
			return nil, fmt.Errorf("read %q response: %w", commandCase.Name, err)
		}

		observations = append(observations, commandObservation{
			name:     commandCase.Name,
			request:  request,
			response: response,
		})
	}

	return observations, nil
}

func compareExchanges(
	direct []commandObservation,
	proxy []commandObservation,
) ([]CommandExchange, error) {
	if len(direct) != len(proxy) {
		return nil, fmt.Errorf(
			"mismatched command result length: direct=%d proxy=%d",
			len(direct),
			len(proxy),
		)
	}

	exchanges := make([]CommandExchange, 0, len(direct))
	for i := range direct {
		directObservation := direct[i]
		proxyObservation := proxy[i]

		if directObservation.name != proxyObservation.name {
			return nil, fmt.Errorf(
				"mismatched command order at index %d: direct=%q proxy=%q",
				i,
				directObservation.name,
				proxyObservation.name,
			)
		}

		requestMatch := directObservation.request == proxyObservation.request
		responseMatch := directObservation.response == proxyObservation.response

		exchanges = append(exchanges, CommandExchange{
			Name:           directObservation.name,
			Request:        directObservation.request,
			DirectResponse: directObservation.response,
			ProxyResponse:  proxyObservation.response,
			Match:          requestMatch && responseMatch,
		})
	}

	return exchanges, nil
}

func normalizedCommandSet(commandSet []CommandCase) []CommandCase {
	if len(commandSet) == 0 {
		return DefaultCommandSet()
	}

	normalized := make([]CommandCase, 0, len(commandSet))
	for index, commandCase := range commandSet {
		name := strings.TrimSpace(commandCase.Name)
		if name == "" {
			name = fmt.Sprintf("command_%d", index+1)
		}

		normalized = append(normalized, CommandCase{
			Name:    name,
			Command: commandCase.Command,
			Data:    commandCase.Data,
		})
	}

	return normalized
}

type mockAdapter struct {
	listener net.Listener
	waiter   sync.WaitGroup
	stopper  sync.Once
}

func startMockAdapter() (*mockAdapter, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen mock adapter: %w", err)
	}

	adapter := &mockAdapter{
		listener: listener,
	}

	adapter.waiter.Add(1)
	go adapter.serve()

	return adapter, nil
}

func (adapter *mockAdapter) Address() string {
	return adapter.listener.Addr().String()
}

func (adapter *mockAdapter) Close() {
	adapter.stopper.Do(func() {
		_ = adapter.listener.Close()
		adapter.waiter.Wait()
	})
}

func (adapter *mockAdapter) serve() {
	defer adapter.waiter.Done()

	for {
		connection, err := adapter.listener.Accept()
		if err != nil {
			if isClosedNetworkError(err) {
				return
			}

			continue
		}

		adapter.waiter.Add(1)
		go adapter.serveConnection(connection)
	}
}

func (adapter *mockAdapter) serveConnection(connection net.Conn) {
	defer adapter.waiter.Done()
	defer connection.Close()

	var request [2]byte
	for {
		if err := maybeSetDeadline(connection); err != nil {
			return
		}

		if _, err := io.ReadFull(connection, request[:]); err != nil {
			if isConnectionClosure(err) {
				return
			}

			return
		}

		command, data, err := southboundenh.DecodeENH(request[0], request[1])
		if err != nil {
			return
		}

		responseCommand, responseData := responseForCommand(command, data)
		response := southboundenh.EncodeENH(responseCommand, responseData)
		if _, err := connection.Write(response[:]); err != nil {
			return
		}
	}
}

func responseForCommand(
	command southboundenh.ENHCommand,
	data byte,
) (southboundenh.ENHCommand, byte) {
	switch command {
	case southboundenh.ENHReqInit:
		return southboundenh.ENHResResetted, data
	case southboundenh.ENHReqStart:
		return southboundenh.ENHResStarted, data
	case southboundenh.ENHReqInfo:
		return southboundenh.ENHResInfo, data
	case southboundenh.ENHReqSend:
		return southboundenh.ENHResReceived, data
	default:
		return southboundenh.ENHResFailed, data
	}
}

type commandProxy struct {
	listener net.Listener
	target   string
	waiter   sync.WaitGroup
	stopper  sync.Once
}

func startCommandProxy(target string) (*commandProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen command proxy: %w", err)
	}

	proxy := &commandProxy{
		listener: listener,
		target:   target,
	}

	proxy.waiter.Add(1)
	go proxy.serve()

	return proxy, nil
}

func (proxy *commandProxy) Address() string {
	return proxy.listener.Addr().String()
}

func (proxy *commandProxy) Close() {
	proxy.stopper.Do(func() {
		_ = proxy.listener.Close()
		proxy.waiter.Wait()
	})
}

func (proxy *commandProxy) serve() {
	defer proxy.waiter.Done()

	for {
		clientConnection, err := proxy.listener.Accept()
		if err != nil {
			if isClosedNetworkError(err) {
				return
			}

			continue
		}

		proxy.waiter.Add(1)
		go proxy.serveConnection(clientConnection)
	}
}

func (proxy *commandProxy) serveConnection(clientConnection net.Conn) {
	defer proxy.waiter.Done()
	defer clientConnection.Close()

	upstreamConnection, err := (&net.Dialer{
		Timeout: defaultDialTimeout,
	}).Dial("tcp", proxy.target)
	if err != nil {
		return
	}
	defer upstreamConnection.Close()

	var request [2]byte
	var response [2]byte

	for {
		if err := maybeSetDeadline(clientConnection); err != nil {
			return
		}
		if err := maybeSetDeadline(upstreamConnection); err != nil {
			return
		}

		if _, err := io.ReadFull(clientConnection, request[:]); err != nil {
			if isConnectionClosure(err) {
				return
			}

			return
		}

		if _, err := upstreamConnection.Write(request[:]); err != nil {
			return
		}

		if _, err := io.ReadFull(upstreamConnection, response[:]); err != nil {
			return
		}

		if _, err := clientConnection.Write(response[:]); err != nil {
			return
		}
	}
}

func maybeSetDeadline(connection net.Conn) error {
	return connection.SetDeadline(time.Now().Add(defaultIOTimeout))
}

func isConnectionClosure(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		isClosedNetworkError(err)
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, net.ErrClosed) {
		return true
	}

	return strings.Contains(err.Error(), "use of closed network connection")
}
