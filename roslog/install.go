package roslog

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ANSI color codes
const (
	ColorReset   = "\033[0m"
	ColorRed     = "\033[31m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorBlue    = "\033[34m"
	ColorMagenta = "\033[35m"
	ColorCyan    = "\033[36m"
	ColorWhite   = "\033[37m"
	ColorBold    = "\033[1m"
	ColorDimGray = "\033[2;37m" // Dim gray for subtle command display
)

// Progress bar state
type InstallProgress struct {
	mu             sync.Mutex
	percentage     int
	statusMessage  string
	currentCommand string
	startTime      time.Time
	isActive       bool
	lastLineLen    int
	lastCmdLineLen int
	hasCmdLine     bool // tracks if we currently have a command line displayed
	spinnerIndex   int
	lastSpinTime   time.Time
}

// Spinner frames for showing activity
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var (
	installProgress *InstallProgress
	progressMu      sync.Mutex
)

// StartInstallProgress initializes and displays the progress bar
func StartInstallProgress() {
	progressMu.Lock()
	defer progressMu.Unlock()

	installProgress = &InstallProgress{
		percentage:    0,
		statusMessage: "Initializing...",
		startTime:     time.Now(),
		isActive:      true,
	}

	// Initial render
	installProgress.render()
}

// UpdateInstallProgress updates the progress percentage
func UpdateInstallProgress(percentage int) {
	if installProgress == nil {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	if percentage < 0 {
		percentage = 0
	}
	if percentage > 100 {
		percentage = 100
	}

	installProgress.percentage = percentage
	installProgress.render()
}

// ShowInstallActivity advances the spinner to show command activity
// Call this whenever a long-running command produces output
func ShowInstallActivity() {
	if installProgress == nil || !installProgress.isActive {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	// Advance spinner
	installProgress.spinnerIndex = (installProgress.spinnerIndex + 1) % len(spinnerFrames)
	installProgress.lastSpinTime = time.Now()
	installProgress.render()
}

// UpdateInstallStatus updates the status message
func UpdateInstallStatus(status string) {
	if installProgress == nil {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	// Clean up the status message
	status = strings.ReplaceAll(status, "_", " ")
	status = toTitleCase(status)

	installProgress.statusMessage = status
	installProgress.render()
}

// SetCurrentCommand sets the currently executing command to display under the progress bar
func SetCurrentCommand(command string) {
	if installProgress == nil {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	installProgress.currentCommand = command
	installProgress.render()
}

// toTitleCase converts a string to title case
func toTitleCase(s string) string {
	s = strings.ToLower(s)
	if len(s) == 0 {
		return s
	}

	words := strings.Fields(s)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(string(word[0])) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

// FinishInstallProgress completes the progress bar
func FinishInstallProgress(success bool) {
	if installProgress == nil {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	installProgress.percentage = 100
	installProgress.isActive = false
	installProgress.render()

	// Print final newline
	fmt.Println()

	if success {
		InstallSuccess("Installation completed successfully!")
	} else {
		InstallError("Installation failed")
	}
}

// render draws the progress bar (must be called with lock held)
func (p *InstallProgress) render() {
	if !p.isActive {
		return
	}

	// If we currently have a command line displayed, move cursor up to the progress bar line
	if p.hasCmdLine {
		fmt.Print("\033[1A\r") // Move up one line and return to start
	}

	// Calculate elapsed time
	elapsed := time.Since(p.startTime)
	elapsedStr := formatDuration(elapsed)

	// Build progress bar
	barWidth := 30
	filled := (p.percentage * barWidth) / 100
	empty := barWidth - filled

	bar := strings.Repeat("█", filled) + strings.Repeat("░", empty)

	// Get current spinner frame
	spinner := spinnerFrames[p.spinnerIndex]

	// Build the line with spinner
	line := fmt.Sprintf("\r%s%s %s[%s%s%s] %3d%% %s| %s%s%s | %s",
		ColorYellow, spinner, // Spinner in yellow
		ColorBold,
		ColorCyan, bar, ColorReset+ColorBold,
		p.percentage,
		ColorReset,
		ColorCyan, p.statusMessage, ColorReset,
		elapsedStr,
	)

	// Calculate visible length (without ANSI codes) for proper clearing
	visibleLen := calculateVisibleLength(line)

	// Clear previous line if it was longer
	if visibleLen < p.lastLineLen {
		// Pad with spaces to clear old content
		line += strings.Repeat(" ", p.lastLineLen-visibleLen)
	}
	p.lastLineLen = visibleLen

	// Build command line (if command is set)
	cmdLine := ""
	willHaveCmdLine := false

	if p.currentCommand != "" {
		// Replace newlines with spaces to keep command on one line
		displayCmd := strings.ReplaceAll(p.currentCommand, "\n", " ")
		// Collapse multiple spaces into one
		displayCmd = strings.Join(strings.Fields(displayCmd), " ")
		// Truncate command if it's too long (max 120 chars)
		if len(displayCmd) > 120 {
			displayCmd = displayCmd[:117] + "..."
		}
		cmdLine = fmt.Sprintf("\n%s  $ %s%s", ColorDimGray, displayCmd, ColorReset)
		cmdVisibleLen := calculateVisibleLength(cmdLine)

		// Clear previous command line if it was longer
		if cmdVisibleLen < p.lastCmdLineLen {
			cmdLine += strings.Repeat(" ", p.lastCmdLineLen-cmdVisibleLen)
		}
		p.lastCmdLineLen = cmdVisibleLen
		willHaveCmdLine = true
	} else if p.lastCmdLineLen > 0 {
		// Clear the command line if no command is set but there was one before
		// Print newline + spaces to clear, then move cursor back up to line 1
		cmdLine = "\n" + strings.Repeat(" ", p.lastCmdLineLen) + "\033[1A\r"
		p.lastCmdLineLen = 0
		willHaveCmdLine = false
	}

	// Write to stdout (progress bar + command line)
	fmt.Print(line + cmdLine)

	// Update the flag AFTER printing so it reflects what we just printed
	p.hasCmdLine = willHaveCmdLine
}

// calculateVisibleLength calculates the visible length of a string without ANSI escape codes
func calculateVisibleLength(s string) int {
	// Remove ANSI escape codes to get visible length
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	visible := ansiRegex.ReplaceAllString(s, "")
	// Also remove carriage return
	visible = strings.ReplaceAll(visible, "\r", "")
	return len(visible)
}

// formatDuration formats a duration nicely
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// InstallInfo prints an info message (moves cursor down to avoid overwriting progress bar)
func InstallInfo(msg string) {
	clearProgressLine()
	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("%s%s %sℹ%s %s\n", ColorBold, timestamp, ColorBlue, ColorReset, msg)
	redrawProgress()
}

// InstallSuccess prints a success message
func InstallSuccess(msg string) {
	clearProgressLine()
	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("%s%s %s✓%s %s\n", ColorBold, timestamp, ColorGreen, ColorReset, msg)
	redrawProgress()
}

// InstallWarning prints a warning message
func InstallWarning(msg string) {
	clearProgressLine()
	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("%s%s %s⚠%s %s\n", ColorBold, timestamp, ColorYellow, ColorReset, msg)
	redrawProgress()
}

// InstallError prints an error message
func InstallError(msg string) {
	clearProgressLine()
	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("%s%s %s✗%s %s\n", ColorBold, timestamp, ColorRed, ColorReset, msg)
	redrawProgress()
}

// InstallDebug prints a debug message (only if progress bar is not active or in debug mode)
func InstallDebug(msg string) {
	if installProgress != nil && installProgress.isActive {
		// Only show in debug mode when progress bar is active
		if !L.Enabled(nil, -4) { // slog.LevelDebug
			return
		}
	}

	clearProgressLine()
	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("%s%s %s🔍%s %s\n", ColorBold, timestamp, ColorMagenta, ColorReset, msg)
	redrawProgress()
}

// clearProgressLine clears the current line if progress bar is active
func clearProgressLine() {
	if installProgress == nil || !installProgress.isActive {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	// Clear the progress bar line
	fmt.Print("\r" + strings.Repeat(" ", installProgress.lastLineLen) + "\r")

	// If there's a command line, move up and clear it
	if installProgress.lastCmdLineLen > 0 {
		// Move cursor up one line, clear it, then move back down
		fmt.Print("\033[1A" + strings.Repeat(" ", installProgress.lastCmdLineLen) + "\r")
	}
}

// redrawProgress redraws the progress bar after printing a message
func redrawProgress() {
	if installProgress == nil || !installProgress.isActive {
		return
	}

	installProgress.mu.Lock()
	defer installProgress.mu.Unlock()

	installProgress.render()
}

// IsInstallProgressActive returns whether the install progress bar is currently active
func IsInstallProgressActive() bool {
	progressMu.Lock()
	defer progressMu.Unlock()
	return installProgress != nil && installProgress.isActive
}

// DisableColors disables color output (for CI/non-TTY environments)
func DisableColors() {
	// This would be implemented by setting all color constants to empty strings
	// For now, we'll detect TTY automatically
}

func init() {
	// Auto-detect if stdout is a TTY
	// If not, we could disable colors automatically
	if !isTTY() {
		// Could disable colors here, but for now we'll keep them
		// since many modern CI systems support ANSI colors
	}
}

// isTTY checks if stdout is a terminal
func isTTY() bool {
	fileInfo, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fileInfo.Mode() & os.ModeCharDevice) != 0
}
