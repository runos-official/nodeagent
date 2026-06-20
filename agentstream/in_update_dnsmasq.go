package agentstream

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// TODO - Restart will cause brief DNS downtime, this is not ideal. We need to find a better solution

// UpdateDnsmasqRequestType is the instruction type that updates dnsmasq configuration.
const UpdateDnsmasqRequestType = "UPDATE_DNSMASQ"

// Global mutex to ensure only one dnsmasq update runs at a time
var dnsmasqUpdateMutex sync.Mutex

const (
	dnsmasqConfigPath = "/etc/dnsmasq.d/runos.conf"
	backupSuffix      = "~"    // Use ~ suffix - ignored by dnsmasq
	tempSuffix        = ".tmp" // Use .tmp extension - ignored by dnsmasq
)

// HandleUpdateDnsmasq decodes an UPDATE_DNSMASQ instruction and rewrites the
// dnsmasq configuration, keeping a backup of the previous file.
func HandleUpdateDnsmasq(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.D("Executing HandleUpdateDnsmasq")

	// Acquire the mutex to prevent concurrent updates
	dnsmasqUpdateMutex.Lock()
	defer dnsmasqUpdateMutex.Unlock()

	type requestType struct {
		FileContents string `json:"fileContents"`
	}
	var request requestType
	if err := commons.JSONB64Decode(instruction.JsonB64, &request); err != nil {
		roslog.E("Error decoding request data", err)
		return nil, err
	}

	// Validate the proposed config against the dnsmasq directive allowlist before
	// it is ever written to disk. This blocks directives that pull external
	// config or run scripts (conf-file, addn-config, dhcp-script, script*, ...).
	if err := validateDnsmasqContents(request.FileContents); err != nil {
		roslog.E("Rejected UPDATE_DNSMASQ: disallowed directive", err)
		return nil, err
	}

	// Check if the new content is different from the existing file
	if !isContentChanged(request.FileContents) {
		roslog.D("dnsmasq configuration unchanged, skipping restart")
		return NoContentResponse, nil
	}

	// Update the dnsmasq configuration file atomically
	if err := updateDnsmasqConfigAtomic(request.FileContents); err != nil {
		roslog.E("Error updating dnsmasq configuration", err)
		return nil, err
	}

	// Restart dnsmasq service
	if err := restartDnsmasqService(); err != nil {
		roslog.E("Error restarting dnsmasq service", err)
		// Try to restore backup if restart fails
		if restoreErr := restoreBackup(); restoreErr != nil {
			roslog.E("Error restoring backup after failed restart", restoreErr)
		}
		return nil, err
	}

	// Clean up backup file on successful completion
	cleanupBackup()

	roslog.D("Successfully updated dnsmasq configuration and restarted service")
	return NoContentResponse, nil
}

// isContentChanged compares the new content with the existing file
func isContentChanged(newContent string) bool {
	// If file doesn't exist, content has changed
	if _, err := os.Stat(dnsmasqConfigPath); os.IsNotExist(err) {
		roslog.D("dnsmasq config file doesn't exist, treating as changed")
		return true
	}

	// Read existing file content
	existingContent, err := ioutil.ReadFile(dnsmasqConfigPath)
	if err != nil {
		roslog.W("Failed to read existing dnsmasq config, treating as changed: %v", err)
		return true
	}

	// Compare using SHA256 hashes for efficiency with large files
	existingHash := sha256.Sum256(existingContent)
	newHash := sha256.Sum256([]byte(newContent))

	changed := !bytes.Equal(existingHash[:], newHash[:])

	if changed {
		roslog.D("dnsmasq configuration content has changed")
	} else {
		roslog.D("dnsmasq configuration content is identical")
	}

	return changed
}

// updateDnsmasqConfigAtomic writes the configuration file atomically with backup
func updateDnsmasqConfigAtomic(fileContents string) error {
	configDir := filepath.Dir(dnsmasqConfigPath)
	tempPath := dnsmasqConfigPath + tempSuffix
	backupPath := dnsmasqConfigPath + backupSuffix

	// Ensure the directory exists
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Create backup of existing file if it exists
	if _, err := os.Stat(dnsmasqConfigPath); err == nil {
		if err := copyFile(dnsmasqConfigPath, backupPath); err != nil {
			return fmt.Errorf("failed to create backup: %w", err)
		}
		roslog.D("Created backup at %s", backupPath)
	}

	// Write to temporary file first (root-only; the final config inherits this
	// mode via the rename below).
	if err := ioutil.WriteFile(tempPath, []byte(fileContents), 0600); err != nil {
		return fmt.Errorf("failed to write temporary file: %w", err)
	}

	// Atomically move temporary file to final location
	if err := os.Rename(tempPath, dnsmasqConfigPath); err != nil {
		// Clean up temp file on failure
		os.Remove(tempPath)
		return fmt.Errorf("failed to move temporary file to final location: %w", err)
	}

	roslog.D("Successfully wrote dnsmasq configuration to %s", dnsmasqConfigPath)
	return nil
}

// copyFile creates a copy of src at dst
func copyFile(src, dst string) error {
	data, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(dst, data, 0600)
}

// restartDnsmasqService restarts the dnsmasq service gracefully
func restartDnsmasqService() error {
	roslog.D("Restarting dnsmasq service")

	// First try systemctl restart (most graceful)
	cmd := exec.Command("systemctl", "restart", "dnsmasq")
	if err := cmd.Run(); err != nil {
		roslog.W("systemctl restart failed, trying service command: %v", err)

		// Fallback to service restart
		cmd = exec.Command("service", "dnsmasq", "restart")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to restart dnsmasq service: %w", err)
		}
	}

	// Wait a moment for the service to fully start
	time.Sleep(2 * time.Second)

	// Verify the service is running
	cmd = exec.Command("systemctl", "is-active", "dnsmasq")
	if err := cmd.Run(); err != nil {
		roslog.W("systemctl is-active failed, trying alternative verification: %v", err)

		// Alternative verification using pgrep
		cmd = exec.Command("pgrep", "dnsmasq")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("dnsmasq service failed to start properly")
		}
	}

	roslog.D("Successfully restarted dnsmasq service")
	return nil
}

// restoreBackup restores the backup file if it exists
func restoreBackup() error {
	backupPath := dnsmasqConfigPath + backupSuffix

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		roslog.W("No backup file found to restore", err)
		return nil
	}

	if err := copyFile(backupPath, dnsmasqConfigPath); err != nil {
		return fmt.Errorf("failed to restore backup: %w", err)
	}

	roslog.D("Successfully restored backup from %s", backupPath)
	return nil
}

// cleanupBackup removes the backup file
func cleanupBackup() {
	backupPath := dnsmasqConfigPath + backupSuffix

	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		roslog.W("Failed to clean up backup file: %v", err)
	} else if err == nil {
		roslog.D("Successfully cleaned up backup file")
	}
}
