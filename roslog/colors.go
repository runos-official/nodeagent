package roslog

import (
	"os"

	"golang.org/x/term"
)

// stdoutIsTTY / stderrIsTTY record whether the respective streams are real
// terminals. They gate ANSI coloring and the carriage-return progress renderer:
// piping `runos ... | tee` or running under CI/systemd must not emit escape
// codes or rewrite lines with \r.
var (
	stdoutIsTTY bool
	stderrIsTTY bool
)

func init() {
	noColor := os.Getenv("NO_COLOR") != ""
	stdoutIsTTY = !noColor && term.IsTerminal(int(os.Stdout.Fd()))
	stderrIsTTY = !noColor && term.IsTerminal(int(os.Stderr.Fd()))

	// Color* is consumed across stdout (install progress) and stderr (Fail).
	// Strip ANSI unless BOTH usable sinks are colorable, so we never leak raw
	// escape codes into a redirected/piped stream.
	if !stdoutIsTTY || !stderrIsTTY {
		DisableColors()
	}
}

// DisableColors blanks every Color* var so all output is plain text. Idempotent;
// safe to call from tests or an explicit --no-color path.
func DisableColors() {
	ColorReset = ""
	ColorRed = ""
	ColorGreen = ""
	ColorYellow = ""
	ColorBlue = ""
	ColorMagenta = ""
	ColorCyan = ""
	ColorWhite = ""
	ColorBold = ""
	ColorDimGray = ""
}

// progressIsTTY reports whether the progress bar may use carriage-return /
// cursor-movement rendering. When false the renderer emits plain newline lines
// so a captured log stays readable.
func progressIsTTY() bool { return stdoutIsTTY }
