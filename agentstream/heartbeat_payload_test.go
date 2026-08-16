package agentstream

import (
	"encoding/json"
	"testing"
)

// TestHeartbeatCarriesRolesKnown pins the heartbeat wire contract that R6 turned
// on. Nodeward decodes these exact field names. rolesKnown is the flag that tells
// Nodeward whether isCp/isWorker may be written: false means the agent could not
// read the node object and is carrying its last known role, so a write would
// demote a live control plane on a transient read failure.
func TestHeartbeatCarriesRolesKnown(t *testing.T) {
	raw, err := json.Marshal(heartbeatRequest{
		ExternalIpAddress: "203.0.113.7",
		IsCp:              true,
		IsWorker:          false,
		Status:            "ready",
		Version:           "1.8.0-rc.16",
		RolesKnown:        true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{"externalIpAddress", "isCp", "isWorker", "status", "version", "rolesKnown"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("heartbeat payload is missing %q; nodeward decodes that exact name", field)
		}
	}

	if decoded["rolesKnown"] != true {
		t.Errorf("rolesKnown = %v, want true", decoded["rolesKnown"])
	}
}

// TestHeartbeatRolesKnownIsAlwaysSent guards the backward-compatibility contract
// from the other side. The field must NOT be omitempty: a new agent that could
// not read the node object has to send rolesKnown=false explicitly, because
// Nodeward reads an ABSENT field as true for old agents.
func TestHeartbeatRolesKnownIsAlwaysSent(t *testing.T) {
	raw, err := json.Marshal(heartbeatRequest{Status: "unknown", RolesKnown: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	value, ok := decoded["rolesKnown"]
	if !ok {
		t.Fatal("rolesKnown was omitted; nodeward would read the absence as true and demote the node")
	}
	if value != false {
		t.Errorf("rolesKnown = %v, want false", value)
	}
}
