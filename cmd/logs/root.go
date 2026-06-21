package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

const logFilePath = "/var/log/runos.log"

var (
	follow   bool
	lines    int
	jsonMode bool
	noColor  bool
)

// LogEntry represents a parsed JSON log entry
type LogEntry struct {
	Time   string         `json:"time"`
	Level  string         `json:"level"`
	Msg    string         `json:"msg"`
	Tag    string         `json:"tag"`
	Source map[string]any `json:"source"`
}

// formatLogLine parses and formats a JSON log line with colors. The bool return
// reports whether the line is non-empty output worth printing (empty/whitespace
// lines are dropped). Unparseable lines are returned verbatim but dimmed so they
// are never silently discarded.
func formatLogLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}

	// In raw/JSON mode the file is already valid JSONL; stream it unchanged.
	if jsonMode {
		return line, true
	}

	// Try to parse as JSON. If it isn't JSON, surface it verbatim (dimmed)
	// rather than dropping it, so nothing is silently lost.
	var entry LogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return fmt.Sprintf("%s%s%s", roslog.ColorDimGray, line, roslog.ColorReset), true
	}

	// Format the timestamp (show only time, not full ISO8601)
	timestamp := entry.Time
	if t, err := time.Parse(time.RFC3339Nano, entry.Time); err == nil {
		timestamp = t.Format("15:04:05.000")
	}

	// Choose color based on log level
	var levelColor string
	var levelText string
	switch strings.ToUpper(entry.Level) {
	case "INFO":
		levelColor = roslog.ColorCyan
		levelText = "INFO "
	case "WARN", "WARNING":
		levelColor = roslog.ColorYellow
		levelText = "WARN "
	case "ERROR":
		levelColor = roslog.ColorRed
		levelText = "ERROR"
	case "DEBUG":
		levelColor = roslog.ColorMagenta
		levelText = "DEBUG"
	default:
		levelColor = roslog.ColorWhite
		levelText = entry.Level
	}

	// Build the formatted output
	var output strings.Builder

	// Timestamp
	output.WriteString(fmt.Sprintf("%s%s%s",
		roslog.ColorDimGray, timestamp, roslog.ColorReset,
	))

	// Tag (if present) - displayed in blue with brackets, right after timestamp
	if entry.Tag != "" {
		// Truncate tag to first 8 characters for readability
		displayTag := entry.Tag
		if len(displayTag) > 8 {
			displayTag = displayTag[:8]
		}
		output.WriteString(fmt.Sprintf(" %s[%s]%s",
			roslog.ColorBlue, displayTag, roslog.ColorReset,
		))
	} else {
		// Add spacing to keep columns aligned when no tag
		output.WriteString("          ") // 10 spaces: " [12345678]"
	}

	// Level
	output.WriteString(fmt.Sprintf(" %s%s%s",
		levelColor, levelText, roslog.ColorReset,
	))

	// Message
	output.WriteString(fmt.Sprintf(" %s%s%s",
		roslog.ColorWhite, entry.Msg, roslog.ColorReset,
	))

	return output.String(), true
}

var RootCmd = &cobra.Command{
	Use:   "logs",
	Short: "Display RunOS Node Agent logs",
	Long: `Display logs from /var/log/runos.log.

By default the last -n entries are pretty-printed with colored levels. Use -f to
follow (tail) the log in real time, and --json to stream the raw JSONL records
unchanged (suitable for piping to jq).`,
	Example: `  # Show the last 50 entries
  runos logs

  # Show the last 200 entries
  runos logs -n 200

  # Follow the log in real time
  runos logs -f

  # Stream raw JSON for further processing
  runos logs --json | jq 'select(.level == "ERROR")'`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if noColor {
			roslog.DisableColors()
		}
		if lines < 1 {
			return roslog.Fail(
				"read node agent logs",
				fmt.Sprintf("invalid -n value %d: must be >= 1", lines),
				"pass a positive count, e.g. `runos logs -n 50`",
			)
		}
		if follow {
			return tailLogs()
		}
		return displayLastLines()
	},
}

func init() {
	RootCmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output (like tail -f)")
	RootCmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of log entries to display")
	RootCmd.Flags().BoolVar(&jsonMode, "json", false, "Stream raw JSONL records unchanged (alias: --raw)")
	RootCmd.Flags().BoolVar(&jsonMode, "raw", false, "Stream raw JSONL records unchanged (alias of --json)")
	RootCmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output")
}

// openLogFile opens the log file or returns an already-reported failure with a
// remedy pointing the operator at the most likely cause.
func openLogFile() (*os.File, error) {
	file, err := os.Open(logFilePath)
	if err != nil {
		return nil, roslog.Fail(
			"read node agent logs",
			fmt.Sprintf("cannot open %s: %v", logFilePath, err),
			"run `runos status`; this file only exists after the agent has run",
		)
	}
	return file, nil
}

// displayLastLines shows the last N displayable entries of the log file using a
// bounded ring buffer, so memory stays proportional to -n rather than the whole
// file. Entries are counted after formatting (so blank lines do not consume a
// slot), and printed to stdout.
func displayLastLines() error {
	file, err := openLogFile()
	if err != nil {
		return err
	}
	defer file.Close()

	// Bounded ring buffer of the last `lines` formatted, displayable entries.
	ring := make([]string, 0, lines)
	start := 0

	scanner := bufio.NewScanner(file)
	// Allow long JSON lines (default 64KiB token limit can truncate big payloads).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		formatted, ok := formatLogLine(scanner.Text())
		if !ok {
			continue
		}
		if len(ring) < lines {
			ring = append(ring, formatted)
		} else {
			ring[start] = formatted
			start = (start + 1) % lines
		}
	}

	if err := scanner.Err(); err != nil {
		return roslog.Fail(
			"read node agent logs",
			fmt.Sprintf("error reading %s: %v", logFilePath, err),
			"check the file is not corrupted; `runos status` confirms the agent state",
		)
	}

	// Emit in chronological order starting at the oldest retained entry.
	for i := 0; i < len(ring); i++ {
		roslog.Println(ring[(start+i)%len(ring)])
	}
	return nil
}

// tailLogs follows the log file in real-time (like tail -f). Diagnostics such as
// the rotation marker go to stderr; log content goes to stdout.
func tailLogs() error {
	// First, display the last N lines.
	if err := displayLastLines(); err != nil {
		return err
	}

	file, err := openLogFile()
	if err != nil {
		return err
	}
	defer file.Close()

	// Seek to the end of the file to start reading new content.
	if _, err = file.Seek(0, io.SeekEnd); err != nil {
		return roslog.Fail(
			"follow node agent logs",
			fmt.Sprintf("cannot seek to end of %s: %v", logFilePath, err),
			"retry; if it persists, check the file is a regular file",
		)
	}

	reader := bufio.NewReader(file)

	// Keep track of last known file size for rotation detection.
	lastFileInfo, _ := file.Stat()
	var lastSize int64
	if lastFileInfo != nil {
		lastSize = lastFileInfo.Size()
	}

	// Poll for new content.
	for {
		line, err := reader.ReadString('\n')

		if err == nil {
			// Successfully read a complete line.
			formatted, ok := formatLogLine(line)
			if ok {
				roslog.Println(formatted)
			}
			continue
		} else if err == io.EOF {
			// Reached end of file, wait and check for new content or rotation.
			time.Sleep(100 * time.Millisecond)

			currentInfo, statErr := os.Stat(logFilePath)
			if statErr != nil {
				return roslog.Fail(
					"follow node agent logs",
					fmt.Sprintf("cannot stat %s: %v", logFilePath, statErr),
					"the log file may have been removed; run `runos status`",
				)
			}

			currentSize := currentInfo.Size()

			// Check if file was rotated (size decreased).
			if currentSize < lastSize {
				// Diagnostic marker goes to stderr, not the log stream on stdout.
				fmt.Fprintln(os.Stderr, "--- Log file rotated, reopening ---")
				file.Close()

				// Reopen the file from the beginning.
				file, err = os.Open(logFilePath)
				if err != nil {
					return roslog.Fail(
						"follow node agent logs",
						fmt.Sprintf("cannot reopen %s after rotation: %v", logFilePath, err),
						"the log file may have been removed; run `runos status`",
					)
				}

				reader = bufio.NewReader(file)
				lastSize = 0
				continue
			}

			// Update last known size.
			lastSize = currentSize
		} else {
			// Some other error occurred.
			return roslog.Fail(
				"follow node agent logs",
				fmt.Sprintf("error reading %s: %v", logFilePath, err),
				"retry; if it persists, check the file is not corrupted",
			)
		}
	}
}
