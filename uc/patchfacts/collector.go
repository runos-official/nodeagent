package patchfacts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Options says which pass is running.
//
// The collector owns no schedule. The caller decides when a pass happens and
// which kind it is, because the slow pass interval belongs to the story that
// reports the record, not to this one.
type Options struct {
	// SlowPass is true on the slow pass. Only a slow pass may refresh the
	// package metadata, and only when no package manager lock is held.
	SlowPass bool
}

// Collector reads the node patch facts from the node it runs on.
//
// Every dependency is a replaceable function value, following this
// repository's own pattern, so a test drives the collector over captured
// command output instead of over a live node.
type Collector struct {
	// run runs one program with its arguments and returns its combined output.
	run func(name string, args ...string) (string, error)
	// readFile reads one file.
	readFile func(path string) ([]byte, error)
	// stat reads one file's metadata.
	stat func(path string) (os.FileInfo, error)
	// readDir lists one directory.
	readDir func(path string) ([]os.DirEntry, error)
	// lockHeld reports whether any package manager lock is held, and which.
	lockHeld func() (bool, string, error)
	// now reads the clock.
	now func() time.Time
}

// New returns a Collector that reads the node it runs on.
func New() *Collector {
	return &Collector{
		run:      runArgv,
		readFile: os.ReadFile,
		stat:     os.Stat,
		readDir:  os.ReadDir,
		lockHeld: packageLockHeld,
		now:      time.Now,
	}
}

// runArgv runs name with args directly, with no shell, and returns the
// combined output.
func runArgv(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// exec runs one row of the command table, with names appended when the row
// takes them. It is the ONLY way this package reaches the node, so a command
// that is not a table row cannot run.
func (c *Collector) exec(key string, names ...string) (string, error) {
	row, err := lookupCommand(key)
	if err != nil {
		return "", err
	}
	args := row.args
	if len(names) > 0 {
		if !row.takesNames {
			return "", fmt.Errorf("patchfacts: command row %q takes no names", key)
		}
		args = append(append([]string{}, row.args...), names...)
	}
	return c.run(row.name, args...)
}

// Collect reads the node and returns one record.
//
// A field the collector cannot read does not fail the call: the field keeps its
// zero or nil value and FieldErrors says why. Collect returns an error only
// when it cannot produce a record at all.
func (c *Collector) Collect(opts Options) (Record, error) {
	var rec Record

	c.collectRunningKernel(&rec)
	c.collectBootTime(&rec)
	c.collectReboot(&rec)
	c.collectInstalledKernels(&rec)
	held, heldKnown := c.collectHeldPackages(&rec)
	c.collectUnitState(&rec)

	// The lock check runs in its own step BEFORE the refresh, so a refusal
	// never follows a write. The refresh then happens only on a slow pass.
	refreshNote := c.refreshMetadata(opts)

	// The age is read after any refresh, so the count is reported with the age
	// of the metadata it was actually computed from.
	ageKnown := c.collectMetadataAge(&rec)
	if !ageKnown && refreshNote != "" {
		rec.FieldErrors["packageMetadataAgeSeconds"] += "; " + refreshNote
	}
	switch {
	case !ageKnown:
		// collectMetadataAge already said why the count is not reported.
	case !heldKnown:
		// The contract excludes every held package from the count, so a count
		// taken while the hold list is unreadable would be too high and would
		// name packages RunOS is keeping where they are.
		rec.noteFieldError("securityUpdateCount", "the held package list is unknown, so the count is not reported: it has to exclude every held package")
	default:
		c.collectSecurityUpdates(&rec, held)
	}

	c.collectLastPackageRun(&rec)

	// A whole second is the resolution the record needs, and it keeps the
	// RFC 3339 text the same length for every node.
	rec.CollectedAt = c.now().UTC().Truncate(time.Second)
	return rec, nil
}

func (c *Collector) collectRunningKernel(rec *Record) {
	out, err := c.exec(cmdUname)
	if err != nil {
		rec.noteFieldError("runningKernel", fmt.Sprintf("uname -r failed: %v", err))
		return
	}
	rec.RunningKernel = strings.TrimSpace(out)
	if rec.RunningKernel == "" {
		rec.noteFieldError("runningKernel", "uname -r printed nothing")
	}
}

func (c *Collector) collectBootTime(rec *Record) {
	b, err := c.readFile(pathProcStat)
	if err != nil {
		rec.noteFieldError("bootTime", fmt.Sprintf("cannot read %s: %v", pathProcStat, err))
		return
	}
	t, err := parseBootTime(string(b))
	if err != nil {
		rec.noteFieldError("bootTime", err.Error())
		return
	}
	rec.BootTime = &t
}

func (c *Collector) collectReboot(rec *Record) {
	// The list is always present, because the flag beside it already says
	// whether a reboot is pending. An empty list is then "no trigger to show",
	// never "RunOS does not know".
	rec.RebootTriggerPackages = []string{}

	info, err := c.stat(pathRebootRequired)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			rec.noteFieldError("rebootRequired", fmt.Sprintf("cannot read %s: %v", pathRebootRequired, err))
		}
		// No flag file is a real answer: no reboot is pending. There is then no
		// onset and no trigger list, and neither is an error.
		return
	}
	rec.RebootRequired = true

	since := info.ModTime().UTC()
	if since.IsZero() {
		rec.noteFieldError("rebootRequiredSince", fmt.Sprintf("%s has no readable modification time", pathRebootRequired))
	} else {
		rec.RebootRequiredSince = &since
	}

	b, err := c.readFile(pathRebootRequiredPkg)
	if err != nil {
		// The flag stays true. An existing flag with no readable package list
		// is a real state, not a reason to drop the reboot fact.
		rec.noteFieldError("rebootTriggerPackages", fmt.Sprintf("cannot read %s: %v", pathRebootRequiredPkg, err))
		return
	}
	if names := parseLines(string(b)); len(names) > 0 {
		rec.RebootTriggerPackages = names
	}
}

func (c *Collector) collectInstalledKernels(rec *Record) {
	out, err := c.exec(cmdDpkgKernels)
	if err != nil {
		rec.noteFieldError("installedKernels", fmt.Sprintf("dpkg-query for the kernel packages failed: %v", err))
		return
	}
	rec.InstalledKernels = parseInstalledKernels(out)
	if len(rec.InstalledKernels) == 0 {
		rec.noteFieldError("installedKernels", "dpkg reports no installed kernel image package")
		return
	}
	rec.NewestInstalledKernel = rec.InstalledKernels[0]
}

// collectHeldPackages fills rec.HeldPackages and returns the held names as a
// set, which the security update count needs. The second result is false when
// the hold list could not be read at all.
func (c *Collector) collectHeldPackages(rec *Record) (map[string]bool, bool) {
	out, err := c.exec(cmdAptMarkShowhold)
	if err != nil {
		rec.noteFieldError("heldPackages", fmt.Sprintf("apt-mark showhold failed: %v", err))
		return nil, false
	}
	names := parseLines(out)
	if len(names) == 0 {
		rec.HeldPackages = []HeldPackage{}
		return map[string]bool{}, true
	}

	set := map[string]bool{}
	safe := make([]string, 0, len(names))
	for _, n := range names {
		set[n] = true
		if safeName(n) {
			safe = append(safe, n)
		} else {
			rec.noteFieldError("heldPackages", fmt.Sprintf("apt-mark showhold named %q, which is not a package name", n))
		}
	}

	versions := map[string]string{}
	if len(safe) > 0 {
		// dpkg-query with no name would list every package, so the call runs
		// only when there is at least one name to ask about.
		vout, err := c.exec(cmdDpkgHeldVersions, safe...)
		if err != nil {
			rec.noteFieldError("heldPackages", fmt.Sprintf("dpkg-query for the held package versions failed: %v", err))
		}
		versions = parseDpkgVersions(vout)
	}

	rec.HeldPackages = make([]HeldPackage, 0, len(names))
	for _, n := range names {
		rec.HeldPackages = append(rec.HeldPackages, HeldPackage{Name: n, Version: versions[n]})
	}
	sort.Slice(rec.HeldPackages, func(i, j int) bool { return rec.HeldPackages[i].Name < rec.HeldPackages[j].Name })
	return set, true
}

func (c *Collector) collectUnitState(rec *Record) {
	rec.AutomaticUpdateUnitState = UnitStateUnknown

	states := make([]unitState, 0, len(automaticUpdateUnits))
	for _, unit := range automaticUpdateUnits {
		out, err := c.exec(cmdSystemctlShow, unit)
		if err != nil {
			rec.noteFieldError("automaticUpdateUnitState", fmt.Sprintf("systemctl show %s failed: %v", unit, err))
			return
		}
		st, err := parseUnitState(out)
		if err != nil {
			rec.noteFieldError("automaticUpdateUnitState", fmt.Sprintf("systemctl show %s: %v", unit, err))
			return
		}
		states = append(states, st)
	}

	periodicOut, err := c.exec(cmdAptConfigPeriodic)
	if err != nil {
		rec.noteFieldError("automaticUpdateUnitState", fmt.Sprintf("apt-config dump APT::Periodic failed: %v", err))
		return
	}
	rec.AutomaticUpdateUnitState = decideUnitState(states, parseAptConfig(periodicOut))
}

// refreshMetadata runs the one writing row, on a slow pass only, and only when
// no package manager lock is held.
//
// The collector stands down when a lock is held rather than waiting it out. It
// then reports the count from the metadata as the metadata already stands.
// A refresh outcome is attached only when the metadata age remains unknown.
// A successful age read never gets a FieldErrors entry.
func (c *Collector) refreshMetadata(opts Options) string {
	if !opts.SlowPass {
		return ""
	}
	held, who, err := c.lockHeld()
	if err != nil {
		return fmt.Sprintf("package metadata was not refreshed because the package lock state could not be read: %v", err)
	}
	if held {
		return fmt.Sprintf("package metadata was not refreshed because another package manager holds %s", who)
	}
	out, err := c.exec(cmdAptRefresh)
	if err != nil {
		return fmt.Sprintf("package metadata refresh failed: %v: %s", err, firstLine(out))
	}
	return ""
}

// collectMetadataAge fills the age, its source and the stale flag. It reports
// whether the age is known, because a count whose metadata age is unknown is
// not reported at all: a count without an age cannot be told apart from the
// permanent zero of a node whose metadata never refreshes.
func (c *Collector) collectMetadataAge(rec *Record) bool {
	rec.SecurityUpdatesStale = true

	when, source, err := c.metadataRefreshedAt()
	if err != nil {
		rec.noteFieldError("packageMetadataAgeSeconds", err.Error())
		rec.noteFieldError("securityUpdateCount", "the package metadata age is unknown, so the count is not reported: a count with no age cannot be told apart from a node whose metadata never refreshes")
		return false
	}

	rec.PackageMetadataSource = source
	age := int64(c.now().UTC().Sub(when).Seconds())
	if age < 0 {
		age = 0
	}
	rec.PackageMetadataAgeSeconds = &age
	rec.SecurityUpdatesStale = time.Duration(age)*time.Second > MetadataStaleAfter
	return true
}

// metadataRefreshedAt returns when the package metadata was last refreshed.
//
// The stamp file is the first source, because an apt hook touches it on every
// SUCCESSFUL refresh, on the node's own clock. The index files are the fallback,
// and a conservative one: apt sets an index file's modification time to the
// server's publication time, which is never later than the fetch, so an age
// taken from it can over-report staleness but never under-report it.
func (c *Collector) metadataRefreshedAt() (time.Time, string, error) {
	if info, err := c.stat(pathUpdateSuccessStamp); err == nil {
		return info.ModTime().UTC(), pathUpdateSuccessStamp, nil
	}

	entries, err := c.readDir(pathAptLists)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("neither %s nor %s can be read: %v", pathUpdateSuccessStamp, pathAptLists, err)
	}
	var newest time.Time
	var newestPath string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Only the index files count. The lock and auxfiles entries carry local
		// times that would read fresher than any index and would hide a node
		// whose metadata never refreshes.
		if !strings.Contains(e.Name(), "_dists_") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
			newestPath = pathAptLists + "/" + e.Name()
		}
	}
	if newestPath == "" {
		return time.Time{}, "", fmt.Errorf("%s is missing and %s holds no readable package index", pathUpdateSuccessStamp, pathAptLists)
	}
	return newest.UTC(), newestPath, nil
}

func (c *Collector) collectSecurityUpdates(rec *Record, held map[string]bool) {
	simOut, err := c.exec(cmdAptSimulateUpdate)
	if err != nil {
		rec.noteFieldError("securityUpdateCount", fmt.Sprintf("the apt upgrade simulation failed: %v: %s", err, firstLine(simOut)))
		return
	}
	waiting := parseSimulatedUpgrade(simOut)

	candidates := make([]string, 0, len(waiting))
	installed := map[string]string{}
	for _, w := range waiting {
		if held[w.name] {
			// Contract: the count excludes every held package, whatever apt
			// would do with it.
			continue
		}
		if !safeName(w.name) {
			rec.noteFieldError("securityUpdateCount", fmt.Sprintf("apt named %q, which is not a package name", w.name))
			return
		}
		candidates = append(candidates, w.name)
		installed[w.name] = w.installedVersion
	}
	if len(candidates) == 0 {
		zero := 0
		rec.SecurityUpdateCount = &zero
		rec.SecurityUpdatePackages = []string{}
		return
	}

	codename, err := c.ubuntuCodename()
	if err != nil {
		rec.noteFieldError("securityUpdateCount", err.Error())
		return
	}

	indexOut, err := c.exec(cmdAptPolicyIndexes)
	if err != nil {
		rec.noteFieldError("securityUpdateCount", fmt.Sprintf("apt-cache policy for the package indexes failed: %v", err))
		return
	}
	indexes := parsePolicyIndexes(indexOut)
	if len(indexes) == 0 {
		rec.noteFieldError("securityUpdateCount", "apt-cache policy named no package index, so a security pocket cannot be recognised")
		return
	}

	policyOut, err := c.exec(cmdAptPolicyPackages, candidates...)
	if err != nil {
		rec.noteFieldError("securityUpdateCount", fmt.Sprintf("apt-cache policy for the waiting packages failed: %v", err))
		return
	}
	versions := parsePolicyPackages(policyOut)

	names := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if isSecurityUpgrade(versions[name], installed[name], indexes, codename) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	count := len(names)
	rec.SecurityUpdateCount = &count
	rec.SecurityUpdatePackages = names
}

// ubuntuCodename reads UBUNTU_CODENAME from /etc/os-release, which is the same
// value the distribution's own update counter takes for the security pocket
// names.
func (c *Collector) ubuntuCodename() (string, error) {
	b, err := c.readFile(pathOSRelease)
	if err != nil {
		return "", fmt.Errorf("cannot read %s, so the security archive cannot be named: %v", pathOSRelease, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "UBUNTU_CODENAME" {
			continue
		}
		if v := strings.Trim(strings.TrimSpace(value), `"'`); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s carries no UBUNTU_CODENAME, so the security archive cannot be named", pathOSRelease)
}

func (c *Collector) collectLastPackageRun(rec *Record) {
	b, err := c.readFile(pathAptHistory)
	if err != nil {
		rec.noteFieldError("lastPackageRunAt", fmt.Sprintf("cannot read %s: %v", pathAptHistory, err))
		return
	}
	if t, ok := lastSuccessfulRun(string(b)); ok {
		rec.LastPackageRunAt = &t
		return
	}

	// The rotated file is read only when the live file holds no finished run.
	rb, err := c.readFile(pathAptHistoryRotated)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			rec.noteFieldError("lastPackageRunAt", fmt.Sprintf("%s holds no finished package run and %s cannot be read: %v", pathAptHistory, pathAptHistoryRotated, err))
			return
		}
		rec.noteFieldError("lastPackageRunAt", fmt.Sprintf("%s holds no package run that finished without an error", pathAptHistory))
		return
	}
	plain, err := gunzip(rb)
	if err != nil {
		rec.noteFieldError("lastPackageRunAt", fmt.Sprintf("cannot read %s: %v", pathAptHistoryRotated, err))
		return
	}
	if t, ok := lastSuccessfulRun(plain); ok {
		rec.LastPackageRunAt = &t
		return
	}
	rec.noteFieldError("lastPackageRunAt", "the apt history holds no package run that finished without an error")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
