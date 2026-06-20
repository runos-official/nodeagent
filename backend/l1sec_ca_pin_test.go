package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/spf13/viper"
)

func TestVerifyL1SecCAPin(t *testing.T) {
	caBytes := []byte("-----BEGIN CERTIFICATE-----\nfake-ca-content\n-----END CERTIFICATE-----\n")
	sum := sha256.Sum256(caBytes)
	correctPin := hex.EncodeToString(sum[:])

	// Save and restore global pin + viper key around the test.
	origPin := l1secCAPinSHA256
	origViper := viper.GetString("mtls.public-ca-sha256")
	t.Cleanup(func() {
		l1secCAPinSHA256 = origPin
		viper.Set("mtls.public-ca-sha256", origViper)
	})

	// No pin configured -> not pinned, no error (backwards compatible).
	l1secCAPinSHA256 = ""
	viper.Set("mtls.public-ca-sha256", "")
	if pinned, err := verifyL1SecCAPin(caBytes); pinned || err != nil {
		t.Errorf("no pin: expected (false, nil), got (%v, %v)", pinned, err)
	}

	// Correct build-time pin -> pinned, no error.
	l1secCAPinSHA256 = correctPin
	if pinned, err := verifyL1SecCAPin(caBytes); !pinned || err != nil {
		t.Errorf("correct build pin: expected (true, nil), got (%v, %v)", pinned, err)
	}

	// Pin with sha256: prefix and uppercase still matches.
	l1secCAPinSHA256 = "SHA256:" + correctPin
	if pinned, err := verifyL1SecCAPin(caBytes); !pinned || err != nil {
		t.Errorf("prefixed pin: expected (true, nil), got (%v, %v)", pinned, err)
	}

	// Wrong build pin -> pinned, error (fail closed).
	l1secCAPinSHA256 = "deadbeef"
	if pinned, err := verifyL1SecCAPin(caBytes); !pinned || err == nil {
		t.Errorf("wrong pin: expected (true, err), got (%v, %v)", pinned, err)
	}

	// Config-based pin (build pin empty) is honored.
	l1secCAPinSHA256 = ""
	viper.Set("mtls.public-ca-sha256", correctPin)
	if pinned, err := verifyL1SecCAPin(caBytes); !pinned || err != nil {
		t.Errorf("config pin: expected (true, nil), got (%v, %v)", pinned, err)
	}

	// Config-based wrong pin fails closed.
	viper.Set("mtls.public-ca-sha256", "deadbeef")
	if pinned, err := verifyL1SecCAPin(caBytes); !pinned || err == nil {
		t.Errorf("config wrong pin: expected (true, err), got (%v, %v)", pinned, err)
	}
}
