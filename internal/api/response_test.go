package api

import (
	"encoding/json"
	"testing"

	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// TestResponseMatchesOutbound guards the wire contract shared by satellites and
// Aether: an sdk.Response and the model.Outbound built from it must marshal to
// identical JSON (plan §3.3 and §3.11).
func TestResponseMatchesOutbound(t *testing.T) {
	tests := []struct {
		name string
		resp sdk.Response
	}{
		{
			name: "recipients and text",
			resp: sdk.Response{
				Recipients: []string{"+15555550102", "+15555550103"},
				Text:       "hello",
				Satellite:  "wind-predictor",
				ThreadID:   "main",
			},
		},
		{
			name: "session action without text",
			resp: sdk.Response{
				Recipients: []string{"+15555550102"},
				Satellite:  "jellyfin",
				Session:    &sdk.SessionAction{Action: "lock", TTLSeconds: 300},
			},
		},
		{
			name: "recipients only",
			resp: sdk.Response{Recipients: []string{"+15555550102"}},
		},
		{
			name: "styled text",
			resp: sdk.Response{
				Recipients: []string{"+15555550102"},
				Text:       "*bold*",
				Format:     "styled",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respJSON, err := json.Marshal(tt.resp)
			if err != nil {
				t.Fatalf("marshal sdk.Response: %v", err)
			}
			outboundJSON, err := json.Marshal(outboundFromResponse(tt.resp))
			if err != nil {
				t.Fatalf("marshal model.Outbound: %v", err)
			}
			if string(respJSON) != string(outboundJSON) {
				t.Fatalf("sdk.Response JSON = %s, model.Outbound JSON = %s", respJSON, outboundJSON)
			}
		})
	}
}
