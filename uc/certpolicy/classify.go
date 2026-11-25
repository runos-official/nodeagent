package certpolicy

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Kind is the kind of outcome of a failed renewal call.
type Kind int

const (
	// WaitUntil: Nodeward names a time before which a renewal is refused.
	WaitUntil Kind = iota + 1
	// Transient: the cause can pass without an operator action on the node
	// identity.
	Transient
	// LostResponse: Nodeward holds a renewal for the held certificate, and
	// the agent does not hold the result.
	LostResponse
	// TerminalForThisCertificate: another caller already renewed the held
	// certificate.
	TerminalForThisCertificate
	// NeedsAgentUpgrade: Nodeward requires a newer agent build.
	NeedsAgentUpgrade
	// CertificateExpired: Nodeward refuses the held certificate as expired.
	CertificateExpired
	// IdentityRepair: Nodeward does not accept the node identity.
	IdentityRepair
	// BadRequest: the agent sent a request that Nodeward can never accept.
	BadRequest
)

var kindNames = map[Kind]string{
	WaitUntil:                  "WaitUntil",
	Transient:                  "Transient",
	LostResponse:               "LostResponse",
	TerminalForThisCertificate: "TerminalForThisCertificate",
	NeedsAgentUpgrade:          "NeedsAgentUpgrade",
	CertificateExpired:         "CertificateExpired",
	IdentityRepair:             "IdentityRepair",
	BadRequest:                 "BadRequest",
}

// AllKinds returns every kind, in declaration order.
func AllKinds() []Kind {
	return []Kind{WaitUntil, Transient, LostResponse, TerminalForThisCertificate,
		NeedsAgentUpgrade, CertificateExpired, IdentityRepair, BadRequest}
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Retries reports whether the agent tries the renewal again on its own
// schedule. Only WaitUntil and Transient retry.
func (k Kind) Retries() bool { return k == WaitUntil || k == Transient }

// ReportResult is the lastResult word of the local renewal state file.
func (k Kind) ReportResult() string {
	switch k {
	case WaitUntil:
		return "waiting"
	case Transient:
		return "transient_failure"
	case CertificateExpired:
		return "expired"
	}
	return "refused"
}

// Prior carries what the caller knows from earlier attempts with the same
// held certificate.
type Prior struct {
	// ResponseMayBeLost is true when an earlier attempt with the held
	// certificate returned an outcome with ResponseMayBeLost true.
	ResponseMayBeLost bool
}

// Outcome is the classified result of one failed renewal call.
type Outcome struct {
	Kind Kind
	// RetryAt is set for WaitUntil only.
	RetryAt time.Time
	// Code is the K2 reason when the refusal has one, otherwise the gRPC
	// code name. It is the lastErrorCode of the state file.
	Code     string
	GRPCCode codes.Code
	// Message is the status text, cut to 512 bytes. It is the
	// lastErrorMessage of the state file.
	Message  string
	NextStep string
	// Unclassified is true when no reason and no table row named the error.
	// The caller logs it.
	Unclassified bool
	// ResponseMayBeLost is true when the status does not prove that
	// Nodeward answered before the mint.
	ResponseMayBeLost bool
}

const maxMessage = 512

// Classify gives the kind of a failed renewal call. The order is: a
// google.rpc.ErrorInfo of Domain with a known reason; else the first table
// row with the same code and text prefix; else the code alone.
func Classify(err error, prior Prior) Outcome {
	st, ok := statusOf(err)
	if !ok {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		return finish(Outcome{
			Kind:              Transient,
			Code:              codes.Unknown.String(),
			GRPCCode:          codes.Unknown,
			Message:           cut(msg),
			Unclassified:      true,
			ResponseMayBeLost: true,
		})
	}
	o := Outcome{GRPCCode: st.Code(), Message: cut(st.Message())}

	if info := errorInfo(st); info != nil {
		if kind, known := knownReasons[info.GetReason()]; known {
			o.Kind = kind
			o.Code = info.GetReason()
			if row := rowFor(st.Code(), info.GetReason()); row != nil {
				o.ResponseMayBeLost = row.MayBeLost
			}
			if kind == TerminalForThisCertificate && prior.ResponseMayBeLost {
				o.Kind = LostResponse
			}
			if kind == WaitUntil {
				at, ok := retryAfter(info.GetMetadata()["retry_after"])
				if !ok {
					at, ok = timeInText(st.Message())
				}
				return finish(waitOrUnclassified(o, at, ok))
			}
			return finish(o)
		}
	}

	for i := range refusals {
		r := &refusals[i]
		if r.Code != st.Code() || !strings.HasPrefix(st.Message(), r.Prefix) {
			continue
		}
		o.Kind = r.Kind
		o.Code = r.Reason
		if o.Code == "" {
			o.Code = r.Code.String()
		}
		o.ResponseMayBeLost = r.MayBeLost
		if r.LostIfPrior && prior.ResponseMayBeLost {
			o.Kind = LostResponse
		}
		if r.Kind == WaitUntil {
			at, ok := timeInText(st.Message())
			return finish(waitOrUnclassified(o, at, ok))
		}
		return finish(o)
	}

	o.Kind = Transient
	o.Code = st.Code().String()
	o.ResponseMayBeLost = true
	switch st.Code() {
	case codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded:
	default:
		o.Unclassified = true
	}
	return finish(o)
}

// statusOf finds the gRPC status in err, also through wrapping. It keeps
// the status text as Nodeward wrote it: status.FromError would replace the
// text of a wrapped status with the whole error text.
func statusOf(err error) (*status.Status, bool) {
	if err == nil {
		return nil, false
	}
	var se interface{ GRPCStatus() *status.Status }
	if errors.As(err, &se) {
		if st := se.GRPCStatus(); st != nil {
			return st, true
		}
	}
	return nil, false
}

// errorInfo returns the first ErrorInfo of Domain, or nil. Details that do
// not decode come back from Details as errors and are skipped.
func errorInfo(st *status.Status) *errdetails.ErrorInfo {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == Domain {
			return info
		}
	}
	return nil
}

// rowFor returns the first table row with the code and the reason.
func rowFor(code codes.Code, reason string) *Refusal {
	for i := range refusals {
		if refusals[i].Code == code && refusals[i].Reason == reason {
			return &refusals[i]
		}
	}
	return nil
}

// waitOrUnclassified sets the WaitUntil time, or makes the outcome
// Transient Unclassified when no time parsed. It never invents a time.
func waitOrUnclassified(o Outcome, at time.Time, ok bool) Outcome {
	if ok {
		o.RetryAt = at
		return o
	}
	o.Kind = Transient
	o.Unclassified = true
	return o
}

// retryAfter parses the retry_after metadata: RFC 3339, or whole unix
// seconds.
func retryAfter(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return time.Time{}, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

// timeInText returns the first RFC 3339 time in a refusal text.
func timeInText(msg string) (time.Time, bool) {
	fields := strings.FieldsFunc(msg, func(r rune) bool {
		return r == ' ' || r == ';' || r == ',' || r == '\t' || r == '\n'
	})
	for _, f := range fields {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// cut shortens s to maxMessage bytes on a rune boundary.
func cut(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	s = s[:maxMessage]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func finish(o Outcome) Outcome {
	o.NextStep = NextStep(o.Kind, o.RetryAt)
	return o
}
