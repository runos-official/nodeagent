package install

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// G30-F3 review: the fetch failure now reports INSTALL_ERROR, so a blip that a retry would have
// carried through must be retried rather than ending the install.
func TestTransientFetchErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"stream not registered yet", errors.New("rpc error: code = Unknown desc = error sending message to node agent: node agent 1234 is not connected"), true},
		{"nodeward restarting", status.Error(codes.Unavailable, "connection closed"), true},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), true},
		{"a real refusal", status.Error(codes.FailedPrecondition, "WAIT_FOR_CONTROL_PLANE: x"), false},
		{"unknown verdict", status.Error(codes.Unknown, "boom"), false},
	}
	for _, c := range cases {
		if got := isTransientFetchError(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
