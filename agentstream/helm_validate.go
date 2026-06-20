package agentstream

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Validation for INSTALL_HELM_CHART. repoUrl/valuesUrl are fetched by helm as
// root, so they must be TLS (https/oci over TLS) and must not resolve to
// internal/metadata addresses. chartName/namespace become argv to helm and are
// constrained to a safe character set.

// helmNameRe constrains chart names and namespaces to lowercase DNS-ish tokens.
var helmNameRe = regexp.MustCompile(`^[a-z0-9._\-]+$`)

// validateHelmName returns an error unless name is a non-empty token of
// lowercase letters, digits, dot, underscore and dash.
func validateHelmName(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len(name) > 253 {
		return fmt.Errorf("%s is too long", field)
	}
	if !helmNameRe.MatchString(name) {
		return fmt.Errorf("%s %q must match ^[a-z0-9._\\-]+$", field, name)
	}
	return nil
}

// validateHelmRepoURL validates a chart repository URL. OCI registries (oci://)
// are allowed and resolve their host through the SSRF guard; http(s) repos must
// be https. In all cases the resolved host must not be loopback/link-local/
// metadata.
func validateHelmRepoURL(repoURL string) error {
	if repoURL == "" {
		return fmt.Errorf("repoUrl is empty")
	}
	if strings.HasPrefix(strings.ToLower(repoURL), "oci://") {
		// Reuse the internal-IP block via a temporary https rewrite so we can
		// resolve and screen the host without requiring a TLS scheme check
		// (OCI implies TLS to the registry).
		u, err := url.Parse(repoURL)
		if err != nil {
			return fmt.Errorf("invalid oci repoUrl: %w", err)
		}
		if u.Hostname() == "" {
			return fmt.Errorf("oci repoUrl has no host")
		}
		ips, _, err := resolveHostIPs(u.Hostname())
		if err != nil {
			return fmt.Errorf("could not resolve oci repoUrl host %q: %w", u.Hostname(), err)
		}
		for _, ip := range ips {
			if isBlockedIP(ip) {
				return fmt.Errorf("refusing oci repoUrl to internal/metadata address %s", ip)
			}
		}
		return nil
	}
	if _, _, err := validateOutboundURL(repoURL, true); err != nil {
		return fmt.Errorf("repoUrl: %w", err)
	}
	return nil
}

// validateHelmValuesURL validates an optional values URL. Empty is allowed
// (no --values). When present it must be https and not internal/metadata.
func validateHelmValuesURL(valuesURL string) error {
	if valuesURL == "" {
		return nil
	}
	if _, _, err := validateOutboundURL(valuesURL, true); err != nil {
		return fmt.Errorf("valuesUrl: %w", err)
	}
	return nil
}

// validateInstallHelmChartRequest runs all helm input validations. It is a pure
// function over the request fields so it can be unit-tested.
func validateInstallHelmChartRequest(repoURL, chartName, namespace, valuesURL string) error {
	if err := validateHelmName("chartName", chartName); err != nil {
		return err
	}
	if err := validateHelmName("namespace", namespace); err != nil {
		return err
	}
	if err := validateHelmRepoURL(repoURL); err != nil {
		return err
	}
	if err := validateHelmValuesURL(valuesURL); err != nil {
		return err
	}
	return nil
}
