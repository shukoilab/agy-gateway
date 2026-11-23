package acp

import (
	"encoding/json"
	"io"
	"testing"
)

func TestTransport_YOLOAutoApprovalPriority(t *testing.T) {
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()

	transport := NewTransport(outWriter, inReader, 1024*1024, nil)
	defer transport.Close()

	// 1. Test "allow_always" priority
	req1 := Request{
		JSONRPC: JSONRPCVersion,
		ID:      int64(1),
		Method:  "session/request_permission",
	}
	params1 := RequestPermissionParams{
		SessionID: "sess_1",
		Options: []PermissionOption{
			{OptionID: "deny_opt", Kind: "deny"},
			{OptionID: "once_opt", Kind: "allow_once"},
			{OptionID: "always_opt", Kind: "allow_always"},
		},
	}
	req1.Params, _ = json.Marshal(params1)
	reqBytes, _ := json.Marshal(req1)

	go func() {
		_, _ = inWriter.Write(append(reqBytes, '\n'))
	}()

	var resp Response
	decoder := json.NewDecoder(outReader)
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	var result RequestPermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to unmarshal permission result: %v", err)
	}

	if result.Outcome.OptionID != "always_opt" {
		t.Fatalf("expected always_opt to be selected, got %q", result.Outcome.OptionID)
	}
}
