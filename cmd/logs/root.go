package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const logFilePath = "/var/log/runos.log"

var (
	follow bool
	lines  int
)

// ANSI color codes optimized for dark backgrounds
const (
	colorReset   = "\033[0m"
	colorGray    = "\033[90m" // Timestamp
	colorCyan    = "\033[96m" // INFO level
	colorYellow  = "\033[93m" // WARN level
	colorRed     = "\033[91m" // ERROR level
	colorMagenta = "\033[95m" // DEBUG level
	colorGreen   = "\033[92m" // SUCCESS
	colorWhite   = "\033[97m" // Message text
	colorBlue    = "\033[94m" // Source info
)

// LogEntry represents a parsed JSON log entry
type LogEntry struct {
	Time   string         `json:"time"`
	Level  string         `json:"level"`
	Msg    string         `json:"msg"`
	Tag    string         `json:"tag"`
	Source map[string]any `json:"source"`
}

// formatLogLine parses and formats a JSON log line with colors
func formatLogLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}

	// Try to parse as JSON
	var entry LogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		// If not JSON, skip this line
		return ""
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
		levelColor = colorCyan
		levelText = "INFO "
	case "WARN", "WARNING":
		levelColor = colorYellow
		levelText = "WARN "
	case "ERROR":
		levelColor = colorRed
		levelText = "ERROR"
	case "DEBUG":
		levelColor = colorMagenta
		levelText = "DEBUG"
	default:
		levelColor = colorWhite
		levelText = entry.Level
	}

	// Build the formatted output
	var output strings.Builder

	// Timestamp
	output.WriteString(fmt.Sprintf("%s%s%s",
		colorGray, timestamp, colorReset,
	))

	// Tag (if present) - displayed in blue with brackets, right after timestamp
	if entry.Tag != "" {
		// Truncate tag to first 8 characters for readability
		displayTag := entry.Tag
		if len(displayTag) > 8 {
			displayTag = displayTag[:8]
		}
		output.WriteString(fmt.Sprintf(" %s[%s]%s",
			colorBlue, displayTag, colorReset,
		))
	} else {
		// Add spacing to keep columns aligned when no tag
		output.WriteString("          ") // 10 spaces: " [12345678]"
	}

	// Level
	output.WriteString(fmt.Sprintf(" %s%s%s",
		levelColor, levelText, colorReset,
	))

	// Message
	output.WriteString(fmt.Sprintf(" %s%s%s",
		colorWhite, entry.Msg, colorReset,
	))

	return output.String()
}

var RootCmd = &cobra.Command{
	Use:   "logs",
	Short: "Display RunOS Node Agent logs",
	Long:  `Display logs from /var/log/runos.log. Use -f to follow (tail) the logs in real-time.`,
	Run: func(cmd *cobra.Command, args []string) {
		if follow {
			tailLogs()
		} else {
			displayLastLines()
		}
	},
}

func init() {
	RootCmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output (like tail -f)")
	RootCmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of lines to display (default 50)")
}

// displayLastLines shows the last N lines of the log file
func displayLastLines() {
	file, err := os.Open(logFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	// Read all lines into a slice
	var allLines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading log file: %v\n", err)
		os.Exit(1)
	}

	// Calculate starting point
	start := 0
	if len(allLines) > lines {
		start = len(allLines) - lines
	}

	// Print the last N lines with formatting
	for i := start; i < len(allLines); i++ {
		formatted := formatLogLine(allLines[i])
		if formatted != "" {
			fmt.Println(formatted)
		}
	}
}

// tailLogs follows the log file in real-time (like tail -f)
func tailLogs() {
	// First, display the last N lines
	displayLastLines()

	// Open the file for reading
	file, err := os.Open(logFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	// Seek to the end of the file to start reading new content
	_, err = file.Seek(0, io.SeekEnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error seeking to end of file: %v\n", err)
		os.Exit(1)
	}

	// Create a reader for the file
	reader := bufio.NewReader(file)

	// Keep track of last known file size for rotation detection
	lastFileInfo, _ := file.Stat()
	lastSize := lastFileInfo.Size()

	// Poll for new content
	for {
		// Try to read a line
		line, err := reader.ReadString('\n')

		if err == nil {
			// Successfully read a complete line
			formatted := formatLogLine(line)
			if formatted != "" {
				fmt.Println(formatted)
			}
			continue
		} else if err == io.EOF {
			// Reached end of file, wait and check for new content or rotation
			time.Sleep(100 * time.Millisecond)

			// Check current file status
			currentInfo, statErr := os.Stat(logFilePath)
			if statErr != nil {
				fmt.Fprintf(os.Stderr, "Error checking log file: %v\n", statErr)
				os.Exit(1)
			}

			currentSize := currentInfo.Size()

			// Check if file was rotated (size decreased)
			if currentSize < lastSize {
				fmt.Println("\n--- Log file rotated, reopening ---")
				file.Close()

				// Reopen the file from the beginning
				file, err = os.Open(logFilePath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reopening log file: %v\n", err)
					os.Exit(1)
				}

				reader = bufio.NewReader(file)
				lastSize = 0
				continue
			}

			// Update last known size
			lastSize = currentSize
		} else {
			// Some other error occurred
			fmt.Fprintf(os.Stderr, "Error reading log file: %v\n", err)
			os.Exit(1)
		}
	}
}
