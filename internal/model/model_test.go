package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestModel(t *testing.T) {
	// Sample inbound JSON
	inboundJSON := []byte(`{"sender":"+15555550102","text":"Hello","provider":"test","group_id":"g1","message_id":"m1"}`)
	var in Inbound
	if err := json.Unmarshal(inboundJSON, &in); err != nil {
		t.Fatalf("unmarshal inbound failed: %v", err)
	}
	if in.Sender != "+15555550102" || in.Text != "Hello" {
		t.Errorf("unexpected inbound fields: %+v", in)
	}

	// Sample outbound JSON (partial fields)
	outboundJSON := []byte(`{"recipients":["+15555550102"],"text":"Hi","satellite":"test"}`)
	var out Outbound
	if err := json.Unmarshal(outboundJSON, &out); err != nil {
		t.Fatalf("unmarshal outbound failed: %v", err)
	}
	if len(out.Recipients) != 1 || out.Text != "Hi" || out.Satellite != "test" {
		t.Errorf("unexpected outbound fields: %+v", out)
	}

	// Round‑trip test for outbound including optional session
	out.Session = &SessionAction{Action: "lock", TTLSeconds: 300}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal outbound failed: %v", err)
	}
	var out2 Outbound
	if err := json.Unmarshal(data, &out2); err != nil {
		t.Fatalf("unmarshal round‑trip failed: %v", err)
	}
	if !reflect.DeepEqual(out, out2) {
		t.Errorf("outbound round‑trip mismatch")
	}
}
