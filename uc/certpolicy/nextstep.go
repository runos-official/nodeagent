package certpolicy

import "time"

// NextStep gives the next step for an outcome kind. at is the RetryAt of a
// WaitUntil and the NotAfter of the held certificate for
// CertificateExpired; the other kinds ignore it. Only the WaitUntil and
// Transient texts say that the agent retries.
func NextStep(k Kind, at time.Time) string {
	switch k {
	case WaitUntil:
		if at.IsZero() {
			return "Nodeward does not renew this certificate yet. The agent retries when Nodeward opens the renewal; no action is needed."
		}
		return "Nodeward does not renew this certificate before " + stamp(at) +
			". The agent retries at that time; no action is needed."
	case Transient:
		return "The renewal failed for a cause that can pass. The agent retries with backoff before the certificate expires; act only if this repeats."
	case LostResponse:
		return "Nodeward renewed this certificate, but the agent did not receive the new certificate. " +
			"The agent checks again when the certificate files change. " +
			"If the certificate gets close to expiry, an operator must re-issue the certificate of this node."
	case TerminalForThisCertificate:
		return "Nodeward already renewed this certificate for another caller. " +
			"The agent checks again when the certificate files change."
	case NeedsAgentUpgrade:
		return "Nodeward requires a newer node agent build to renew this certificate. " +
			"Update the node agent from the Console or with the RunOS CLI."
	case CertificateExpired:
		return ExpiredRemedy(at)
	case IdentityRepair:
		return "Nodeward does not accept the identity of this node (removed, revoked or unknown). " +
			"An operator must check this node in the Console."
	case BadRequest:
		return "This node agent build sent a renewal request that Nodeward refuses. " +
			"Update the node agent from the Console or with the RunOS CLI."
	}
	return "The renewal failed. An operator must check this node in the Console."
}

// ExpiredRemedy is the one source of the remedy text for an expired node
// certificate. A zero notAfter gives the text with no time.
func ExpiredRemedy(notAfter time.Time) string {
	when := ""
	if !notAfter.IsZero() {
		when = " at " + stamp(notAfter)
	}
	return "The certificate of this node expired" + when + ". " +
		"An operator must re-issue the certificate of this node. " +
		"If that is not possible, remove the node and register the node again with a new token from the Console; " +
		"the node then gets a new identity."
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
