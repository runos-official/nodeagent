package maintenance

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

const (
	statusCompleted = "completed"
	statusFailed    = "failed"

	reasonDryRun            = "dry-run"
	reasonInstallFailed     = "install-failed"
	reasonInstallUnverified = "install-unverified"
	reasonNoCandidate       = "no-candidate"
	reasonNotInstalled      = "not-installed"
	reasonNotNewerRevision  = "not-newer-revision"
	reasonUnholdFailed      = "unhold-failed"
	reasonUnreadableVersion = "unreadable-version"
)

var packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

var controlledPackages = func() []string {
	packages := append([]string{}, commons.RuntimeHeldPackages...)
	return append(packages, "runc", "etcd-client")
}()

type packageRunner interface {
	installedVersion(name string) (version string, installed bool, err error)
	candidateVersion(name string) (version string, available bool, err error)
	setHold(name string, hold bool) error
	installVersion(name, version string) error
	isHeld(name string) (bool, error)
}

type packageResult struct {
	Name             string `json:"name"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	CandidateVersion string `json:"candidateVersion,omitempty"`
	Moved            bool   `json:"moved"`
	Reason           string `json:"reason,omitempty"`
	HeldAfter        bool   `json:"heldAfter"`
	diagnostic       string
}

type packageReport struct {
	Status   string          `json:"status"`
	Packages []packageResult `json:"packages"`
}

type systemPackageRunner struct{}

func (systemPackageRunner) command(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r systemPackageRunner) installedVersion(name string) (string, bool, error) {
	out, err := r.command("dpkg-query", "-W", "-f=${db:Status-Status}\\t${Version}", name)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("dpkg-query %s: %w: %s", name, err, out)
	}
	status, version, ok := strings.Cut(out, "\t")
	if !ok || status != "installed" || version == "" {
		return "", false, fmt.Errorf("dpkg-query %s returned unreadable state %q", name, out)
	}
	return version, true, nil
}

func (r systemPackageRunner) candidateVersion(name string) (string, bool, error) {
	out, err := r.command("apt-cache", "policy", name)
	if err != nil {
		return "", false, fmt.Errorf("apt-cache policy %s: %w: %s", name, err, out)
	}
	return parseCandidateVersion(out)
}

func parseCandidateVersion(output string) (string, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Candidate:") {
			continue
		}
		candidate := strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
		if candidate == "(none)" {
			return "", false, nil
		}
		if candidate == "" || strings.ContainsAny(candidate, " \t") {
			return "", false, fmt.Errorf("apt-cache returned unreadable candidate %q", candidate)
		}
		return candidate, true, nil
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("reading apt-cache output: %w", err)
	}
	return "", false, errors.New("apt-cache output has no Candidate line")
}

func (r systemPackageRunner) setHold(name string, hold bool) error {
	action := "unhold"
	if hold {
		action = "hold"
	}
	out, err := r.command("apt-mark", action, name)
	if err != nil {
		return fmt.Errorf("apt-mark %s %s: %w: %s", action, name, err, out)
	}
	return nil
}

func (r systemPackageRunner) installVersion(name, version string) error {
	out, err := r.command("apt-get", "install", "--yes", "--only-upgrade", "--no-remove", name+"="+version)
	if err != nil {
		return fmt.Errorf("apt-get install %s: %w: %s", name, err, out)
	}
	return nil
}

func (r systemPackageRunner) isHeld(name string) (bool, error) {
	out, err := r.command("apt-mark", "showhold")
	if err != nil {
		return false, fmt.Errorf("apt-mark showhold: %w: %s", err, out)
	}
	for _, held := range strings.Fields(out) {
		if held == name {
			return true, nil
		}
	}
	return false, nil
}

func defaultPackageList() []string {
	return append([]string{}, commons.RuntimeHeldPackages...)
}

func parsePackageList(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	packages := make([]string, 0, len(parts))
	for _, name := range parts {
		if name == "" {
			return nil, errors.New("package list contains an empty entry")
		}
		if !packageNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid package name %q", name)
		}
		if slices.Contains(commons.KubernetesHeldPackages, name) {
			return nil, fmt.Errorf("package %q moves only through a Kubernetes upgrade", name)
		}
		if !slices.Contains(controlledPackages, name) {
			return nil, fmt.Errorf("package %q is outside the controlled maintenance set", name)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("package %q appears more than once", name)
		}
		seen[name] = struct{}{}
		packages = append(packages, name)
	}
	return packages, nil
}

func runPackages(runner packageRunner, packages []string, dryRun bool) (packageReport, error) {
	report := packageReport{Status: statusCompleted, Packages: make([]packageResult, 0, len(packages))}
	var runErrors []error
	for _, name := range packages {
		result, err := runPackage(runner, name, dryRun)
		report.Packages = append(report.Packages, result)
		if err != nil {
			runErrors = append(runErrors, err)
		}
	}
	if len(runErrors) != 0 {
		report.Status = statusFailed
	}
	return report, errors.Join(runErrors...)
}

func runPackage(runner packageRunner, name string, dryRun bool) (result packageResult, runErr error) {
	result.Name = name
	installed, present, err := runner.installedVersion(name)
	if err != nil {
		result.Reason = reasonUnreadableVersion
		result.diagnostic = err.Error()
		return result, fmt.Errorf("read installed version for %s: %w", name, err)
	}
	if !present {
		result.Reason = reasonNotInstalled
		return result, nil
	}
	result.InstalledVersion = installed

	candidate, available, err := runner.candidateVersion(name)
	if err != nil {
		result.Reason = reasonUnreadableVersion
		result.diagnostic = err.Error()
		return finishWithoutMutation(runner, result, fmt.Errorf("read candidate version for %s: %w", name, err))
	}
	if !available {
		result.Reason = reasonNoCandidate
		return finishWithoutMutation(runner, result, nil)
	}
	result.CandidateVersion = candidate

	move, err := commons.IsSameUpstreamNewerRevision(installed, candidate)
	if err != nil {
		result.Reason = reasonUnreadableVersion
		result.diagnostic = err.Error()
		return finishWithoutMutation(runner, result, fmt.Errorf("compare versions for %s: %w", name, err))
	}
	if !move {
		result.Reason = reasonNotNewerRevision
		return finishWithoutMutation(runner, result, nil)
	}
	if dryRun {
		result.Reason = reasonDryRun
		return finishWithoutMutation(runner, result, nil)
	}

	// Register the re-hold before the unhold. An unhold can fail after apt has
	// already changed state, so every return below must first restore the hold.
	defer func() {
		holdErr := runner.setHold(name, true)
		held, readErr := runner.isHeld(name)
		result.HeldAfter = held
		if holdErr != nil {
			result.diagnostic = joinDiagnostic(result.diagnostic, holdErr.Error())
		}
		if readErr != nil {
			result.diagnostic = joinDiagnostic(result.diagnostic, readErr.Error())
			runErr = errors.Join(runErr, fmt.Errorf("read final hold for %s: %w", name, readErr))
			return
		}
		if !held {
			runErr = errors.Join(runErr, fmt.Errorf("package %s is not held after maintenance", name))
		}
	}()

	if err := runner.setHold(name, false); err != nil {
		result.Reason = reasonUnholdFailed
		result.diagnostic = err.Error()
		return result, nil
	}
	if err := runner.installVersion(name, candidate); err != nil {
		result.Reason = reasonInstallFailed
		result.diagnostic = err.Error()
		return result, nil
	}
	actual, present, err := runner.installedVersion(name)
	if err != nil {
		result.Reason = reasonInstallUnverified
		result.diagnostic = err.Error()
		return result, fmt.Errorf("read installed version after maintenance for %s: %w", name, err)
	}
	if !present || actual != candidate {
		result.Reason = reasonInstallUnverified
		result.diagnostic = fmt.Sprintf("installed version after maintenance is %q, want %q", actual, candidate)
		return result, nil
	}
	result.Moved = true
	return result, nil
}

func finishWithoutMutation(runner packageRunner, result packageResult, prior error) (packageResult, error) {
	held, err := runner.isHeld(result.Name)
	result.HeldAfter = held
	if err != nil {
		result.diagnostic = joinDiagnostic(result.diagnostic, err.Error())
		return result, errors.Join(prior, fmt.Errorf("read final hold for %s: %w", result.Name, err))
	}
	if !held {
		return result, errors.Join(prior, fmt.Errorf("package %s is not held after maintenance", result.Name))
	}
	return result, prior
}

func joinDiagnostic(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func newRootCmd(runner packageRunner) *cobra.Command {
	root := &cobra.Command{
		Use:   "maintenance",
		Short: "Run controlled node maintenance operations",
	}
	var packageList string
	var dryRun bool
	var jsonOutput bool
	packagesCmd := &cobra.Command{
		Use:   "packages",
		Short: "Move held packages within their current upstream version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			packages := defaultPackageList()
			var validationErr error
			if cmd.Flags().Changed("packages") {
				packages, validationErr = parsePackageList(packageList)
			}
			if validationErr != nil {
				report := packageReport{Status: statusFailed, Packages: []packageResult{}}
				if err := writeReport(cmd, report, jsonOutput); err != nil {
					return err
				}
				cmd.PrintErrln(validationErr)
				return roslog.AlreadyReported(validationErr)
			}

			report, runErr := runPackages(runner, packages, dryRun)
			if err := writeReport(cmd, report, jsonOutput); err != nil {
				return err
			}
			for _, result := range report.Packages {
				if result.diagnostic != "" {
					cmd.PrintErrf("%s: %s\n", result.Name, result.diagnostic)
				}
			}
			if runErr != nil {
				cmd.PrintErrln(runErr)
				return roslog.AlreadyReported(runErr)
			}
			return nil
		},
	}
	packagesCmd.Flags().StringVar(&packageList, "packages", "", "Comma-separated package list")
	packagesCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report decisions without changing packages")
	packagesCmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON result object")
	root.AddCommand(packagesCmd)
	return root
}

func writeReport(cmd *cobra.Command, report packageReport, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Package maintenance: %s\n", report.Status)
	for _, result := range report.Packages {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: moved=%t held=%t", result.Name, result.Moved, result.HeldAfter)
		if result.Reason != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " reason=%s", result.Reason)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return nil
}

// RootCmd is the maintenance command registered by the node agent CLI.
var RootCmd = newRootCmd(systemPackageRunner{})
