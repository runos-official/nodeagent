package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
)

// b64JSON marshals v to JSON and base64-encodes it, the wire shape every
// instruction payload uses.
func b64JSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := commons.JSONB64Encode(v)
	if err != nil {
		t.Fatalf("JSONB64Encode: %v", err)
	}
	return encoded
}

// TestHandleApplyCR_BadBase64 verifies that an invalid base64 JsonB64 is
// rejected at the decode boundary (returning an error, no nil-deref) before any
// cluster I/O is attempted.
func TestHandleApplyCR_BadBase64(t *testing.T) {
	in := &pb.ToNodeAgent{Type: ApplyCRRequestType, Tag: "t1", JsonB64: "!!!not-base64!!!"}
	resp, err := HandleApplyCR(in)
	if err == nil {
		t.Fatal("expected error for invalid base64 payload, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on decode error, got %+v", resp)
	}
}

// TestHandleApplyCR_BadJSON verifies that valid base64 holding non-JSON is
// rejected at the unmarshal boundary.
func TestHandleApplyCR_BadJSON(t *testing.T) {
	in := &pb.ToNodeAgent{
		Type:    ApplyCRRequestType,
		Tag:     "t2",
		JsonB64: base64.StdEncoding.EncodeToString([]byte("this is not json")),
	}
	resp, err := HandleApplyCR(in)
	if err == nil {
		t.Fatal("expected error for non-JSON payload, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on decode error, got %+v", resp)
	}
}

// TestHandleApplyCR_BadInnerCR verifies the second decode seam: a well-formed
// request whose crB64 field is not valid base64 must fail in applyCR before any
// resource is applied.
func TestHandleApplyCR_BadInnerCR(t *testing.T) {
	in := &pb.ToNodeAgent{
		Type:    ApplyCRRequestType,
		Tag:     "t3",
		JsonB64: b64JSON(t, map[string]string{"crB64": "!!!not-base64!!!"}),
	}
	resp, err := HandleApplyCR(in)
	if err == nil {
		t.Fatal("expected error for invalid inner crB64, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on inner decode error, got %+v", resp)
	}
}

// TestHandleDeleteCR_BadBase64 verifies the delete handler rejects an invalid
// payload at the decode boundary before any cluster I/O.
func TestHandleDeleteCR_BadBase64(t *testing.T) {
	in := &pb.ToNodeAgent{Type: DeleteCRRequestType, Tag: "t4", JsonB64: "%%%"}
	resp, err := HandleDeleteCR(in)
	if err == nil {
		t.Fatal("expected error for invalid base64 payload, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on decode error, got %+v", resp)
	}
}

// TestHandleRemoveEtcdMember_BadBase64 verifies the etcd-member handler rejects
// an invalid payload at the decode boundary before any etcdctl invocation.
func TestHandleRemoveEtcdMember_BadBase64(t *testing.T) {
	in := &pb.ToNodeAgent{Type: RemoveEtcdMemberRequestType, Tag: "t5", JsonB64: "@@@"}
	resp, err := HandleRemoveEtcdMember(in)
	if err == nil {
		t.Fatal("expected error for invalid base64 payload, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on decode error, got %+v", resp)
	}
}

// TestHandleRunKubectlCommand_BadJSON verifies the kubectl handler rejects a
// payload that is not a JSON array of args at the decode boundary.
func TestHandleRunKubectlCommand_BadJSON(t *testing.T) {
	// Valid base64, but the JSON is an object where a []string is required.
	payload := base64.StdEncoding.EncodeToString([]byte(`{"not":"an array"}`))
	in := &pb.ToNodeAgent{Type: RunKubectlCommandRequestType, Tag: "t9", JsonB64: payload}
	resp, err := HandleRunKubectlCommand(in)
	if err == nil {
		t.Fatal("expected error for non-array kubectl payload, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response on decode error, got %+v", resp)
	}
}

// TestApplyCRRequestDecode_RoundTrip is a focused check that the applyCRRequest
// wire shape (crB64 field) decodes as expected, guarding the JSON tag.
func TestApplyCRRequestDecode_RoundTrip(t *testing.T) {
	want := "Y3IteWFtbA==" // base64("cr-yaml")
	encoded := b64JSON(t, map[string]string{"crB64": want})

	var got applyCRRequest
	if err := commons.JSONB64Decode(encoded, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CrB64 != want {
		t.Fatalf("CrB64 = %q, want %q", got.CrB64, want)
	}

	// Sanity: the field really is JSON-tagged "crB64".
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := asMap["crB64"]; !ok {
		t.Fatalf("expected JSON key crB64 in %s", raw)
	}
}
