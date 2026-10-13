package certificate

import (
	"context"
	"crypto"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/csrcert"
	"github.com/runos-official/nodeagent/uc/registernode"
)

// issued is an accepted RegenerateCertificate response, not yet on disk.
type issued struct {
	outcome csrcert.Outcome
	key     *csrcert.Key
	resp    csrcert.Response
}

// requestCertificate asks nodeward for a certificate for a fresh local key
// (FPL315). The current mtls key signs the proof of possession. The response
// content decides the path: an old nodeward returns a key, which is used as
// before. Errors never cause a legacy request. Nothing is written here.
func requestCertificate(ctx context.Context, c l2sec.NodewardClient, p registernode.CredPaths, now func() time.Time) (*issued, error) {
	current, err := tls.LoadX509KeyPair(p.Cert, p.Key)
	if err != nil {
		return nil, fmt.Errorf("load current certificate and key: %w", err)
	}
	signer, ok := current.PrivateKey.(crypto.Signer)
	if !ok || len(current.Certificate) == 0 {
		return nil, fmt.Errorf("current key cannot sign a mint proof")
	}
	key, err := csrcert.NewKey()
	if err != nil {
		return nil, err
	}

	var resp *l2sec.RegenerateCertificateResponse
	err = csrcert.CallWithProof(signer, current.Certificate[0], l2sec.Nodeward_RegenerateCertificate_FullMethodName, key, now,
		func(proof *l2sec.MintProof) error {
			var callErr error
			resp, callErr = c.RegenerateCertificate(ctx, &l2sec.RegenerateCertificateRequest{
				CsrPem: key.CSRPEM(),
				Proof:  proof,
			})
			return callErr
		})
	if err != nil {
		return nil, err
	}

	r := csrcert.Response{
		CertificatePEM: resp.CertificatePem,
		CACertPEM:      resp.CaCert,
		PrivateKey:     resp.PrivateKey,
		PublicKey:      resp.PublicKey,
	}
	outcome, err := csrcert.Resolve(key, r, now())
	if err != nil {
		return nil, err
	}
	roslog.I("Renewal response accepted", "path", outcome.String())
	return &issued{outcome: outcome, key: key, resp: r}, nil
}

// writeCertificates writes an accepted response to disk.
func writeCertificates(got *issued, p registernode.CredPaths) error {
	if err := registernode.WriteCredentials(got.outcome, got.key, got.resp, p); err != nil {
		return err
	}
	roslog.I("New certificates written to disk")
	return nil
}
