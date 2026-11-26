package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/acp"
)

// TestTransport_LargePayloadWithoutErrTooLong verifies that the ACP transport
// can stream and decode massive lines (exceeding old 16 MiB bufio.Scanner limits)
// without ErrTooLong fatal pipe termination.
func TestTransport_LargePayloadWithoutErrTooLong(t *testing.T) {
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()

	// Initialize transport with 128 MiB buffer ceiling
	transport := acp.NewTransport(outWriter, inReader, 134217728, nil)
	defer transport.Close()

	// Construct a massive 20 MiB string payload (exceeds previous 16 MiB ceiling)
	largeText := strings.Repeat("M", 20*1024*1024)

	go func() {
		// Read outgoing request from outReader
		buf := make([]byte, 2048)
		_, _ = outReader.Read(buf)

		// Respond with matching ID: 1 containing the 20 MiB payload
		resp := acp.Response{
			JSONRPC: acp.JSONRPCVersion,
			ID:      int64(1),
			Result:  json.RawMessage(fmt.Sprintf(`{"large_text":%q}`, largeText)),
		}
		rawBytes, _ := json.Marshal(resp)
		_, _ = inWriter.Write(append(rawBytes, '\n'))
	}()

	var result struct {
		LargeText string `json:"large_text"`
	}

	// 20 MiB through %q-escaped JSON marshalling plus a chunked pipe copy takes ~14s under
	// -race instrumentation, so 10s raced the kernel and flaked the suite. Generous ceiling.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	err := transport.Call(ctx, "session/prompt", nil, &result)
	if err != nil {
		t.Fatalf("expected transport.Call to handle 20 MiB line without ErrTooLong, got err: %v", err)
	}

	if len(result.LargeText) != 20*1024*1024 {
		t.Fatalf("expected large text length %d, got %d", 20*1024*1024, len(result.LargeText))
	}
}
