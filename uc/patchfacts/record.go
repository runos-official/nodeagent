package patchfacts

import "time"

// The automatic update unit states. The wire form is snake_case, because the
// conductor ingest route, the read endpoint and the surfaces all validate and
// render these exact words (Contract A4 of objective 108's header).
//
// There is no "managed" state: nothing in any RunOS repository writes an
// automatic update configuration, so no node can report one.
const (
	// UnitStateMasked means every automatic package unit is masked, so nothing
	// on the node installs a package on its own.
	UnitStateMasked = "masked"

	// UnitStateEnabledOutsideRunOS means at least one automatic package unit is
	// loaded and enabled or active, AND the apt periodic settings would make it
	// install. RunOS leaves such a node alone.
	UnitStateEnabledOutsideRunOS = "enabled_outside_runos"

	// UnitStateUnknown means the collector could not read the unit state or the
	// periodic settings. It is never reported as a state the node is in.
	UnitStateUnknown = "unknown"
)

// MetadataStaleAfter is the age at which the package metadata behind the
// security update count is reported stale.
//
// The number is 24 hours because the distribution's own refresh job runs daily,
// so a day is the freshness the distribution itself treats as normal, and a
// tighter bound would mark a healthy node stale between two slow passes.
//
// This is NOT the 60 minute FACT staleness of the node status contract. That
// one measures how old the node's reading is; this one measures how old the
// node's package list is. The two must not be merged.
const MetadataStaleAfter = 24 * time.Hour

// HeldPackage is one package held with apt-mark hold, and the version it is
// held at.
type HeldPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Record is the node patch facts record: one record per node, per collection.
//
// Every pointer field distinguishes "the node reported this value" from "RunOS
// does not know". A nil SecurityUpdateCount means the collector could not read
// the count, and the server reads it as Unknown. A count of zero means the node
// reported zero. The collector NEVER sends zero for a count it could not read.
//
// FieldErrors carries one entry per field the collector could not read, keyed
// by the field's JSON name. Without it, one unreadable file would force a
// choice between discarding the whole record and reporting a zero, and
// reporting a zero is exactly the false "Up to date" this work exists to
// remove. With it, the server shows Unknown for one field and the true value
// for the rest.
type Record struct {
	// CollectedAt is when the collector finished this record.
	CollectedAt time.Time `json:"collectedAt"`

	// RunningKernel is the kernel release the node runs now, as uname -r gives
	// it, for example "6.8.0-147-generic".
	RunningKernel string `json:"runningKernel"`

	// InstalledKernels are the kernel releases installed on the node, newest
	// first, in the same vocabulary as RunningKernel. Kernel meta packages are
	// excluded, and so is any package row dpkg reports as not installed.
	InstalledKernels []string `json:"installedKernels"`

	// NewestInstalledKernel is the first entry of InstalledKernels, empty when
	// the list could not be read.
	NewestInstalledKernel string `json:"newestInstalledKernel"`

	// RebootRequired is true when the node carries the distribution's
	// reboot-required flag.
	RebootRequired bool `json:"rebootRequired"`

	// RebootRequiredSince is when the reboot became pending: the modification
	// time of the reboot-required flag file. The file sits on a memory file
	// system that the boot clears, so the time is always after the boot and
	// always on the node's clock.
	//
	// It is nil when no reboot is pending, or when the flag's time could not be
	// read. A nil onset makes the server's reboot-pending age nil. The server
	// must never fall back to the node's uptime, because a node booted a week
	// ago whose package run asked for a reboot ten minutes ago would then read
	// seven days.
	RebootRequiredSince *time.Time `json:"rebootRequiredSince"`

	// RebootTriggerPackages are the packages that asked for the reboot. It is
	// empty when none is readable; an existing flag file with no readable
	// package list keeps RebootRequired true and records the read error.
	RebootTriggerPackages []string `json:"rebootTriggerPackages"`

	// SecurityUpdateCount is how many waiting updates are security updates,
	// with every held package excluded. Nil means RunOS does not know.
	SecurityUpdateCount *int `json:"securityUpdateCount"`

	// SecurityUpdatePackages are the names behind SecurityUpdateCount, sorted.
	SecurityUpdatePackages []string `json:"securityUpdatePackages"`

	// SecurityUpdatesStale is true when the package metadata the count came
	// from is older than MetadataStaleAfter, or when its age could not be read.
	SecurityUpdatesStale bool `json:"securityUpdatesStale"`

	// PackageMetadataAgeSeconds is the age of the package metadata the count
	// was computed from. Nil means the age could not be read, and then
	// SecurityUpdatesStale is true and SecurityUpdateCount is nil.
	PackageMetadataAgeSeconds *int64 `json:"packageMetadataAgeSeconds"`

	// PackageMetadataSource is the path the age was read from.
	PackageMetadataSource string `json:"packageMetadataSource"`

	// LastPackageRunAt is the end time of the newest package manager run that
	// finished without an error. Nil when none is readable.
	LastPackageRunAt *time.Time `json:"lastPackageRunAt"`

	// BootTime is when the node booted. Nil when it could not be read.
	BootTime *time.Time `json:"bootTime"`

	// AutomaticUpdateUnitState is one of UnitStateMasked,
	// UnitStateEnabledOutsideRunOS or UnitStateUnknown.
	AutomaticUpdateUnitState string `json:"automaticUpdateUnitState"`

	// HeldPackages are the packages held with apt-mark hold, with the version
	// each is held at, sorted by name.
	HeldPackages []HeldPackage `json:"heldPackages"`

	// FieldErrors maps a field's JSON name to why the collector could not read
	// it. It is nil when the collector read every field.
	FieldErrors map[string]string `json:"fieldErrors"`
}

// noteFieldError records why one field could not be read. It never overwrites
// an earlier note for the same field, so the first and most specific reason
// survives.
func (r *Record) noteFieldError(field, reason string) {
	if r.FieldErrors == nil {
		r.FieldErrors = map[string]string{}
	}
	if _, seen := r.FieldErrors[field]; !seen {
		r.FieldErrors[field] = reason
	}
}
