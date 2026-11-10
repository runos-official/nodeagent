package install

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/runos-official/nodeagent/version"
)

// nodeAgentVersionFieldNumber is the field number the install-commands request
// carries the node agent version on. The control plane reads the same number,
// and the two repositories hand-sync this proto with no gate between them, so
// the number is pinned here as well as in the generated file.
const nodeAgentVersionFieldNumber = 10

// The control plane gates the runtime package holds on the node agent version
// and reads it from this REQUEST, not from the stored node record: the record is
// filled by the heartbeat, and a node being installed may not have sent one yet.
// So the request must carry the version, on the agreed field number, on the wire.
func TestInstallCommandsRequestCarriesTheNodeAgentVersion(t *testing.T) {
	req := newGetInstallCommandsRequest("ZmlsZS1saXN0")

	// Read it back off the wire, not off the struct, because the wire is what
	// the control plane decodes.
	wire, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	got, ok := stringFieldOnWire(t, wire, nodeAgentVersionFieldNumber)
	if !ok {
		t.Fatalf("the request carries no field %d, so the control plane gate reads no version and skips the runtime holds on every fresh node", nodeAgentVersionFieldNumber)
	}
	if got != version.Version {
		t.Errorf("field %d = %q, want this agent's own version %q", nodeAgentVersionFieldNumber, got, version.Version)
	}
	if got == "" {
		t.Errorf("field %d is empty; the gate must fail closed on an empty version, so sending one is never right", nodeAgentVersionFieldNumber)
	}

	// The field the request already had must survive.
	if req.GetJsonB64FileList() != "ZmlsZS1saXN0" {
		t.Errorf("the netplan file list was lost: %q", req.GetJsonB64FileList())
	}
}

// stringFieldOnWire finds a length-delimited field by number in a marshalled
// message and returns it as a string. It reads the wire format directly so the
// test does not depend on the generated getter, which is the other half of the
// definition this story must keep in agreement.
func stringFieldOnWire(t *testing.T, wire []byte, number int) (string, bool) {
	t.Helper()
	for i := 0; i < len(wire); {
		tag, n := decodeVarint(wire[i:])
		if n == 0 {
			t.Fatalf("the marshalled request is not readable at byte %d", i)
		}
		i += n
		fieldNumber := int(tag >> 3)
		switch tag & 0x7 {
		case 0: // varint
			_, n := decodeVarint(wire[i:])
			if n == 0 {
				t.Fatalf("truncated varint at byte %d", i)
			}
			i += n
		case 2: // length-delimited
			length, n := decodeVarint(wire[i:])
			if n == 0 {
				t.Fatalf("truncated length at byte %d", i)
			}
			i += n
			if i+int(length) > len(wire) {
				t.Fatalf("length %d at byte %d runs past the end", length, i)
			}
			value := string(wire[i : i+int(length)])
			i += int(length)
			if fieldNumber == number {
				return value, true
			}
		case 5:
			i += 4
		case 1:
			i += 8
		default:
			t.Fatalf("unexpected wire type %d for field %d", tag&0x7, fieldNumber)
		}
	}
	return "", false
}

func decodeVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
