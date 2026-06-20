package commons

import (
	"bufio"
	"os"
	"strings"
)

// GetOSInfo returns the OS name and version in format "name-version" (e.g., "ubuntu-24.04")
func GetOSInfo() string {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	defer file.Close()

	var osName, osVersion string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "ID=") {
			osName = strings.Trim(strings.TrimPrefix(line, "ID="), "\"")
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			osVersion = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"")
		}
	}

	if osName == "" {
		return "unknown"
	}

	if osVersion != "" {
		return osName + "-" + osVersion
	}
	return osName
}
