package certpolicy

import "google.golang.org/grpc/codes"

// Domain is the google.rpc.ErrorInfo domain of a Nodeward renewal refusal.
const Domain = "nodeward.runos.com"

// K2 reasons that Nodeward attaches to a renewal refusal.
const (
	ReasonRenewalNotOpen       = "CERT_RENEWAL_NOT_OPEN"
	ReasonRepeatBlocked        = "CERT_RENEWAL_REPEAT_BLOCKED"
	ReasonAlreadyRegenerated   = "CERT_ALREADY_REGENERATED"
	ReasonMintCheckUnavailable = "CERT_MINT_CHECK_UNAVAILABLE"
	ReasonLegacyMintOff        = "CERT_LEGACY_MINT_OFF"
	ReasonCSRRequired          = "CERT_CSR_REQUIRED"
	ReasonTestRenewalRefused   = "CERT_TEST_RENEWAL_REFUSED"
)

// knownReasons gives the kind of each K2 reason. Each kind equals the kind
// of the table row of the same refusal (ruling H2).
var knownReasons = map[string]Kind{
	ReasonRenewalNotOpen:       WaitUntil,
	ReasonRepeatBlocked:        WaitUntil,
	ReasonAlreadyRegenerated:   TerminalForThisCertificate,
	ReasonMintCheckUnavailable: Transient,
	ReasonLegacyMintOff:        NeedsAgentUpgrade,
	ReasonCSRRequired:          NeedsAgentUpgrade,
	ReasonTestRenewalRefused:   Transient,
}

// Refusal is one row of the refusal table.
type Refusal struct {
	// Item is the number of the refusal in the renewal refusal inventory
	// (1 to 16). The test refusal row has 0.
	Item int
	Code codes.Code
	// Prefix is the start of the status text. An empty prefix matches any
	// text of the code.
	Prefix string
	Kind   Kind
	// Reason is the K2 reason of the same refusal, or "".
	Reason string
	// LostIfPrior gives LostResponse in place of Kind when an earlier
	// attempt with the same certificate may have lost its response.
	LostIfPrior bool
	// MayBeLost is true when the status does not prove that Nodeward
	// answered before the mint.
	MayBeLost bool
}

// refusals is the bounded set of renewal refusals, checked against the
// Nodeward source. The first row with the same code and a matching prefix
// wins, so a longer prefix comes before a shorter one, and the rows with an
// empty prefix come last.
var refusals = []Refusal{
	{Item: 1, Code: codes.FailedPrecondition, Prefix: "certificate is not inside its renewal window; renewal opens at ", Kind: WaitUntil, Reason: ReasonRenewalNotOpen},
	{Item: 2, Code: codes.FailedPrecondition, Prefix: "certificate was regenerated too recently; renew again after ", Kind: WaitUntil, Reason: ReasonRepeatBlocked},
	{Item: 3, Code: codes.FailedPrecondition, Prefix: "certificate was already regenerated", Kind: TerminalForThisCertificate, Reason: ReasonAlreadyRegenerated, LostIfPrior: true},
	{Item: 4, Code: codes.FailedPrecondition, Prefix: "could not check the minting rules", Kind: Transient, Reason: ReasonMintCheckUnavailable},
	// Nodeward also answers this text when the mint lock fails, and the
	// lock can fail after the handler minted. So the response may be lost.
	{Item: 4, Code: codes.Unavailable, Prefix: "could not check the minting rules", Kind: Transient, Reason: ReasonMintCheckUnavailable, MayBeLost: true},
	{Item: 5, Code: codes.FailedPrecondition, Prefix: "legacy key minting through nodehub is turned off", Kind: NeedsAgentUpgrade, Reason: ReasonLegacyMintOff},
	{Item: 6, Code: codes.FailedPrecondition, Prefix: "this agent must renew with a certificate signing request", Kind: NeedsAgentUpgrade, Reason: ReasonCSRRequired},
	{Item: 7, Code: codes.InvalidArgument, Prefix: "mint proof sent without a certificate signing request", Kind: BadRequest},
	{Item: 9, Code: codes.InvalidArgument, Prefix: "invalid certificate signing request: key is the presented certificate key", Kind: BadRequest},
	{Item: 8, Code: codes.InvalidArgument, Prefix: "invalid certificate signing request: ", Kind: BadRequest},
	{Item: 10, Code: codes.InvalidArgument, Prefix: "mint proof is missing or malformed", Kind: BadRequest},
	{Item: 11, Code: codes.PermissionDenied, Prefix: "mint proof signature does not verify", Kind: BadRequest},
	// The proof call already retried once with the server time. A later
	// attempt can pass once the node clock is right.
	{Item: 12, Code: codes.FailedPrecondition, Prefix: "mint proof time is outside the 5 minute window", Kind: Transient},
	{Item: 13, Code: codes.Unavailable, Prefix: "could not check the mint proof", Kind: Transient},
	// Each attempt makes a new nonce.
	{Item: 14, Code: codes.PermissionDenied, Prefix: "mint proof nonce was already used", Kind: Transient},
	{Item: 15, Code: codes.Unauthenticated, Prefix: "certificate has expired", Kind: CertificateExpired},
	{Item: 15, Code: codes.Unauthenticated, Prefix: "certificate not found in database", Kind: IdentityRepair},
	{Item: 15, Code: codes.Unauthenticated, Prefix: "certificate has been revoked", Kind: IdentityRepair},
	{Item: 15, Code: codes.Unauthenticated, Prefix: "unknown certificate version", Kind: IdentityRepair},
	{Item: 15, Code: codes.Unauthenticated, Prefix: "invalid legacy certificate format", Kind: IdentityRepair},
	{Item: 15, Code: codes.NotFound, Prefix: "node not found", Kind: IdentityRepair},
	{Item: 15, Code: codes.Unavailable, Prefix: "authentication check failed", Kind: Transient},
	// The test refusal of a DEV test certificate life (K5). The agent keeps
	// trying until the certificate expires.
	{Item: 0, Code: codes.FailedPrecondition, Prefix: "certificate renewal is refused by a test setting", Kind: Transient, Reason: ReasonTestRenewalRefused},
	// Item 16: the gateway call gate and burst limit. The gateway refuses
	// ResourceExhausted before Nodeward sees the call. An Unavailable with
	// an unknown text can hide a broken connection after a mint.
	{Item: 16, Code: codes.ResourceExhausted, Prefix: "", Kind: Transient},
	{Item: 16, Code: codes.Unavailable, Prefix: "", Kind: Transient, MayBeLost: true},
}

// Refusals returns a copy of the refusal table, in match order.
func Refusals() []Refusal {
	out := make([]Refusal, len(refusals))
	copy(out, refusals)
	return out
}
