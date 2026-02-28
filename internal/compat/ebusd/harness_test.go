package ebusd

import (
	"context"
	"testing"
	"time"

	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func TestRunConfigOnlyMigrationHarnessMatchesDirectAndProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := RunConfigOnlyMigrationHarness(ctx, nil)
	if err != nil {
		t.Fatalf("expected harness success, got %v", err)
	}

	if result.AdapterEndpoint == result.ProxyEndpoint {
		t.Fatalf(
			"expected distinct adapter and proxy endpoints, got %q",
			result.AdapterEndpoint,
		)
	}

	if !result.Compatible() {
		t.Fatalf("expected compatible direct/proxy responses")
	}

	expectedCommandSet := DefaultCommandSet()
	if len(result.Exchanges) != len(expectedCommandSet) {
		t.Fatalf(
			"expected %d command exchanges, got %d",
			len(expectedCommandSet),
			len(result.Exchanges),
		)
	}

	for index, exchange := range result.Exchanges {
		expectedCommand := expectedCommandSet[index]
		expectedRequest := southboundenh.EncodeENH(expectedCommand.Command, expectedCommand.Data)
		expectedResponseCommand, expectedResponseData := responseForCommand(
			expectedCommand.Command,
			expectedCommand.Data,
		)
		expectedResponse := southboundenh.EncodeENH(expectedResponseCommand, expectedResponseData)

		if exchange.Name != expectedCommand.Name {
			t.Fatalf(
				"expected exchange name %q at index %d, got %q",
				expectedCommand.Name,
				index,
				exchange.Name,
			)
		}

		if exchange.Request != expectedRequest {
			t.Fatalf(
				"expected request %#v at index %d, got %#v",
				expectedRequest,
				index,
				exchange.Request,
			)
		}

		if exchange.DirectResponse != expectedResponse {
			t.Fatalf(
				"expected direct response %#v at index %d, got %#v",
				expectedResponse,
				index,
				exchange.DirectResponse,
			)
		}

		if exchange.ProxyResponse != expectedResponse {
			t.Fatalf(
				"expected proxy response %#v at index %d, got %#v",
				expectedResponse,
				index,
				exchange.ProxyResponse,
			)
		}

		if !exchange.Match {
			t.Fatalf("expected exchange %q to match", exchange.Name)
		}
	}
}

func TestRunConfigOnlyMigrationHarnessUsesFailureResponseForUnknownCommand(t *testing.T) {
	commandSet := []CommandCase{
		{
			Name:    "unknown_command",
			Command: 0x0F,
			Data:    0x55,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := RunConfigOnlyMigrationHarness(ctx, commandSet)
	if err != nil {
		t.Fatalf("expected harness success for unknown command fallback, got %v", err)
	}

	if len(result.Exchanges) != 1 {
		t.Fatalf("expected one command exchange, got %d", len(result.Exchanges))
	}

	exchange := result.Exchanges[0]
	expectedResponse := southboundenh.EncodeENH(southboundenh.ENHResFailed, 0x55)

	if exchange.DirectResponse != expectedResponse {
		t.Fatalf(
			"expected direct failed response %#v, got %#v",
			expectedResponse,
			exchange.DirectResponse,
		)
	}

	if exchange.ProxyResponse != expectedResponse {
		t.Fatalf(
			"expected proxy failed response %#v, got %#v",
			expectedResponse,
			exchange.ProxyResponse,
		)
	}

	if !exchange.Match {
		t.Fatalf("expected unknown-command exchange to match across direct and proxy")
	}
}
