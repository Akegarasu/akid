package protocol

import (
	"bytes"
	"encoding/json"
	"testing"

	"akid/internal/logging"
)

func TestMaximumLogChunkFitsProtocolFrame(t *testing.T) {
	// []byte is base64-encoded by encoding/json, so arbitrary binary content
	// expands by about 4/3 rather than JSON's 6x string escape worst case.
	response := Response{
		Protocol: Version,
		ID:       json.RawMessage("1"),
		Result: logging.LogChunk{
			Data: bytes.Repeat([]byte{0}, logging.MaxReadSize),
		},
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxMessageSize {
		t.Fatalf("maximum log response is %d bytes, protocol limit is %d", len(data), MaxMessageSize)
	}
}
