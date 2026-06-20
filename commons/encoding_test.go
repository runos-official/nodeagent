package commons

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

type encodingSample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Tags  []string
}

func TestJSONB64EncodeDecode_RoundTripStruct(t *testing.T) {
	in := encodingSample{Name: "node-a", Count: 7, Tags: []string{"x", "y"}}

	encoded, err := JSONB64Encode(in)
	if err != nil {
		t.Fatalf("JSONB64Encode: %v", err)
	}

	// The encoded payload must be valid base64.
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		t.Fatalf("encoded output is not valid base64: %v", err)
	}

	var out encodingSample
	if err := JSONB64Decode(encoded, &out); err != nil {
		t.Fatalf("JSONB64Decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

func TestJSONB64EncodeDecode_RoundTripMap(t *testing.T) {
	in := map[string]any{
		"peers": []any{"a", "b"},
		"nid":   "xxxxx",
	}

	encoded, err := JSONB64Encode(in)
	if err != nil {
		t.Fatalf("JSONB64Encode: %v", err)
	}

	out := map[string]any{}
	if err := JSONB64Decode(encoded, &out); err != nil {
		t.Fatalf("JSONB64Decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

func TestJSONB64Decode_Errors(t *testing.T) {
	// Valid base64 of a JSON object {"name":"ok","count":1}.
	validB64 := mustB64(t, `{"name":"ok","count":1}`)

	cases := []struct {
		name    string
		encoded string
		target  any
		wantErr string
	}{
		{
			name:    "malformed base64",
			encoded: "not!base64!!",
			target:  &encodingSample{},
			wantErr: "error decoding base64",
		},
		{
			name:    "valid base64 but malformed JSON",
			encoded: mustB64(t, `{"name": "oops"`),
			target:  &encodingSample{},
			wantErr: "error decoding JSON",
		},
		{
			name:    "nil target",
			encoded: validB64,
			target:  nil,
			wantErr: "error decoding JSON",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := JSONB64Decode(tc.encoded, tc.target)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error to contain %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func mustB64(t *testing.T, s string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(s))
}
