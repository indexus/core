package wire

import (
	"encoding/json"
)

type Item struct {
	RequestID  string          `json:"request_id,omitempty"`
	Collection string          `json:"collection"`
	Location   string          `json:"location"`
	Id         string          `json:"id"`
	Metrics    []float64       `json:"metrics"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	// Weight is how many items a pre-aggregated record stands for (0: one).
	Weight float64 `json:"weight,omitempty"`
	// Binary says Payload is the JSON string of the base64 of raw bytes;
	// /records?format=binary sends the bytes themselves.
	Binary bool `json:"binary,omitempty"`
}
