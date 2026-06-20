package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// L1Sec public CA pinning.
//
// The L1Sec public CA (l1sec-ca.runos.public.pem) is downloaded by the installer
// over a CDN URL with no integrity check, then used as the TLS root for the
// registration channel. A MITM of that fetch could substitute its own CA, letting
// it impersonate nodeward during registration (which exchanges the mTLS client
// cert) -> root RCE. To close that, the agent verifies the on-disk CA against an
// expected sha256 BEFORE trusting it.
//
// The expected hash is supplied at build time via:
//
//	go build -ldflags "-X github.com/runos-official/nodeagent/backend.l1secCAPinSHA256=<hex>"
//
// or per-deployment via config key `mtls.public-ca-sha256`. The build-time pin is
// preferred (it travels with the attested binary). The repo cannot vendor the
// production PEM, so it is pinned by hash rather than //go:embed.
//
// Fail policy: if a pin is configured and the CA does not match, registration is
// aborted (fail closed). If NO pin is configured, the CA is used as before but a
// loud warning is logged, so an unpinned deployment is visibly degraded rather
// than silently insecure (this preserves legitimate behavior on already-deployed
// nodes that predate the pin).

// l1secCAPinSHA256 is the expected lowercase-hex sha256 of the DER/PEM bytes of
// the L1Sec public CA file. Empty by default; set via -ldflags at release time.
var l1secCAPinSHA256 = ""

// configuredL1SecCAPin returns the effective pin: the build-time value if set,
// otherwise the config override. The result is lowercased and trimmed.
func configuredL1SecCAPin() string {
	pin := strings.TrimSpace(l1secCAPinSHA256)
	if pin == "" {
		pin = strings.TrimSpace(viper.GetString("mtls.public-ca-sha256"))
	}
	return strings.TrimPrefix(strings.ToLower(pin), "sha256:")
}

// verifyL1SecCAPin checks caBytes (the raw bytes read from the CA file) against
// the configured pin. It returns (pinned, err):
//   - pinned reports whether a pin was configured at all,
//   - err is non-nil only when a pin WAS configured and the hash did not match.
//
// Callers must abort when err != nil. When pinned is false, callers should log a
// warning but may proceed (backwards-compatible with unpinned deployments).
func verifyL1SecCAPin(caBytes []byte) (pinned bool, err error) {
	pin := configuredL1SecCAPin()
	if pin == "" {
		return false, nil
	}
	sum := sha256.Sum256(caBytes)
	got := hex.EncodeToString(sum[:])
	if got != pin {
		return true, fmt.Errorf("l1sec CA pin mismatch: file sha256 %s does not match pinned %s", got, pin)
	}
	return true, nil
}
