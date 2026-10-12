package backend

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"

	"github.com/runos-official/nodeagent/roslog"
)

// runosOIDArc is the private arc nodeward uses for the version and type
// extensions on agent certificates (1.3.6.1.4.1.99999.1.1 and .1.2).
var runosOIDArc = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999}

// isRunOSExtension reports whether oid is the RunOS arc or sits under it.
func isRunOSExtension(oid asn1.ObjectIdentifier) bool {
	if len(oid) < len(runosOIDArc) {
		return false
	}
	return oid[:len(runosOIDArc)].Equal(runosOIDArc)
}

// verifyL2SecServerLeaf is the T-50 check (FPL315, F5 path). It runs as
// tls.Config.VerifyConnection, so after the normal CA and server-name checks.
//
// The agent CA signs the L2SEC server certificate and every agent
// certificate, and agent certificates carry ServerAuth. A server leaf that
// also has ClientAuth, or any RunOS extension, is an agent certificate, so
// the agent refuses it. Nodeward's real server certificate has neither.
func verifyL2SecServerLeaf(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("T-50: server presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	if reason := agentLeafReason(leaf); reason != "" {
		err := fmt.Errorf("T-50: refusing server certificate %q: %s; it looks like an agent certificate, not the nodeward server certificate", leaf.Subject.CommonName, reason)
		roslog.E("Refused the L2SEC server certificate (T-50)", err,
			"server_name", cs.ServerName, "subject", leaf.Subject.String(), "serial", leaf.SerialNumber.String())
		return err
	}
	return nil
}

// agentLeafReason returns why leaf must not be trusted as a server, or "".
func agentLeafReason(leaf *x509.Certificate) string {
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			return "it has the ClientAuth extended key usage"
		}
	}
	for _, ext := range leaf.Extensions {
		if isRunOSExtension(ext.Id) {
			return fmt.Sprintf("it carries the RunOS extension %s", ext.Id)
		}
	}
	return ""
}
