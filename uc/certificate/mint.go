package certificate

import (
	"context"
	"crypto"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert"
)

// issued is an accepted RegenerateCertificate response, not yet on disk.
type issued struct {
	outcome csrcert.Outcome
	key     *csrcert.Key
	resp    csrcert.Response
}

// requestCertificate is one mint attempt. It asks nodeward for a
// certificate for a fresh local key (FPL315). The held mtls key signs the
// proof of possession. The response content decides the path: an old
// nodeward returns a key, which is used as before. Errors never cause a
// legacy request. Nothing is written here. callTimeout gives the timeout of
// each wire call.
func requestCertificate(ctx context.Context, c MintClient, p certstore.Paths, now func() time.Time, callTimeout func() time.Duration) (*issued, error) {
	current, err := certstore.LoadKeyPair(p.Cert, p.Key)
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
			callCtx, cancel := context.WithTimeout(ctx, callTimeout())
			defer cancel()
			var callErr error
			resp, callErr = c.RegenerateCertificate(callCtx, &l2sec.RegenerateCertificateRequest{
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

// material gives the child for the certificate store: the local key on the
// certificate outcome, the returned key on the legacy outcome.
func (g *issued) material() (certstore.Material, error) {
	switch g.outcome {
	case csrcert.OutcomeCertificate:
		keyPEM, err := g.key.PKCS8PEM()
		if err != nil {
			return certstore.Material{}, err
		}
		return certstore.Material{KeyPEM: keyPEM, CertPEM: []byte(g.resp.CertificatePEM), CAPEM: []byte(g.resp.CACertPEM)}, nil
	case csrcert.OutcomeLegacy:
		return certstore.Material{KeyPEM: []byte(g.resp.PrivateKey), CertPEM: []byte(g.resp.PublicKey), CAPEM: []byte(g.resp.CACertPEM)}, nil
	}
	return certstore.Material{}, fmt.Errorf("no credentials to adopt (outcome %v)", g.outcome)
}
