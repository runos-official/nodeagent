package roslog

import (
	"fmt"
	"io"
	"os"
)

// stderr is the sink for operator-facing failure output. It is a package var so
// tests can capture it; operator errors go to stderr (not stdout) so that a
// `runos install >/dev/null` still surfaces failures.
var stderr io.Writer = os.Stderr

// SupportLine is appended to operator-facing failure output (Fail, and the
// preflight summary) so a user who hits a genuine RunOS bug — rather than an
// unmet environment prerequisite — knows how to reach us. Centralized here so
// the wording is identical everywhere it appears, and appears exactly once per
// failure report.
const SupportLine = "If you believe this is a RunOS problem rather than an environment issue, email support@runos.com with the output above and we will fix it ASAP."

// Fail prints the canonical operator-facing failure block to stderr AND records
// it as a structured error log line (durable, visible in `runos logs`). It is
// the single failure-reporting primitive used by the install/register paths so
// every failure looks the same and is never silently swallowed.
//
// Terminal block (cooperates with the install progress bar):
//
//	✗ FAILED: <step>
//	  Cause: <cause>
//	  Try:   <remedy>
//
// The caller is still responsible for returning a non-zero error / exiting; Fail
// only reports. Returns an error wrapping step+cause so callers can do
// `return roslog.Fail(...)` directly from a cobra RunE.
func Fail(step, cause, remedy string) error {
	// Durable structured record first (lands in /var/log/runos.log as JSON).
	E("install step failed", nil, "step", step, "cause", cause, "remedy", remedy)

	// Then the human-readable block on stderr, coexisting with the progress bar.
	clearProgressLine()
	fmt.Fprintf(stderr, "\n%s%s✗ FAILED:%s %s\n", ColorBold, ColorRed, ColorReset, step)
	if cause != "" {
		fmt.Fprintf(stderr, "  Cause: %s\n", cause)
	}
	if remedy != "" {
		fmt.Fprintf(stderr, "  Try:   %s\n", remedy)
	}
	fmt.Fprintf(stderr, "  Full log: %s\n", logFilePath)
	fmt.Fprintf(stderr, "  %s\n", SupportLine)
	redrawProgress()

	return fmt.Errorf("%s: %s", step, cause)
}
