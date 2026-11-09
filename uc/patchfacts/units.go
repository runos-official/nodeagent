package patchfacts

import (
	"fmt"
	"strings"
)

// unitState is one automatic package unit's state, as systemctl show reports
// it.
type unitState struct {
	id            string
	loadState     string
	unitFileState string
	activeState   string
}

// masked reports whether systemd will refuse to start this unit.
func (u unitState) masked() bool {
	return u.loadState == "masked" || u.unitFileState == "masked"
}

// couldRun reports whether this unit schedules work or is running now.
func (u unitState) couldRun() bool {
	if u.masked() {
		return false
	}
	switch u.unitFileState {
	case "enabled", "enabled-runtime":
		return true
	}
	return u.activeState == "active" || u.activeState == "activating"
}

// parseUnitState reads the key and value list of
// `systemctl show --property=Id,LoadState,UnitFileState,ActiveState <unit>`.
//
// The output is one Key=Value line per property, measured on two running Ubuntu
// 24.04 nodes. A masked unit reads LoadState=masked, UnitFileState=masked and
// ActiveState=inactive; a unit systemd acts on reads LoadState=loaded,
// UnitFileState=enabled and ActiveState=active.
func parseUnitState(out string) (unitState, error) {
	var st unitState
	var any bool
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		any = true
		switch key {
		case "Id":
			st.id = value
		case "LoadState":
			st.loadState = value
		case "UnitFileState":
			st.unitFileState = value
		case "ActiveState":
			st.activeState = value
		}
	}
	if !any || st.loadState == "" {
		return unitState{}, fmt.Errorf("the output carries no LoadState line")
	}
	return st, nil
}

// decideUnitState reduces the unit states and the apt periodic settings to one
// member of the three-member enum.
//
// BOTH READS ARE NEEDED, and a measurement shows why. On both RunOS nodes read
// for this story, every automatic unit was masked while the apt periodic
// settings still carried Ubuntu's defaults, Update-Package-Lists "1" and
// Unattended-Upgrade "1". Reading only the settings would call such a node
// enabled outside RunOS, which is wrong: nothing runs. Reading only the units
// would call a node with an enabled timer and every periodic setting at zero
// enabled outside RunOS, which is also wrong: that node installs nothing.
//
// So the rule is: every unit masked gives masked. Otherwise a unit systemd
// would act on, together with a periodic setting that would install, gives
// enabled outside RunOS. Anything else gives unknown, because a loaded unit
// whose settings install nothing is not a state this enum has a word for, and
// a guess there would read as a fact.
func decideUnitState(states []unitState, periodic map[string]string) string {
	if len(states) == 0 {
		return UnitStateUnknown
	}

	allMasked := true
	anyCouldRun := false
	for _, st := range states {
		if !st.masked() {
			allMasked = false
		}
		if st.couldRun() {
			anyCouldRun = true
		}
	}
	if allMasked {
		return UnitStateMasked
	}
	if anyCouldRun && periodicWouldInstall(periodic) {
		return UnitStateEnabledOutsideRunOS
	}
	return UnitStateUnknown
}

// periodicWouldInstall reports whether the apt periodic settings would make the
// automatic units install a package.
//
// APT::Periodic::Enable is absent by default and absent means enabled, so only
// an explicit zero turns the whole mechanism off. APT::Periodic::Unattended-
// Upgrade is the setting that decides whether anything is installed, and it is
// zero or absent on a node that only refreshes its package lists.
func periodicWouldInstall(periodic map[string]string) bool {
	if v, ok := periodic["APT::Periodic::Enable"]; ok && isZeroSetting(v) {
		return false
	}
	v, ok := periodic["APT::Periodic::Unattended-Upgrade"]
	return ok && !isZeroSetting(v)
}

// isZeroSetting reports whether an apt periodic value turns its setting off.
// apt reads these as integers, where zero and an empty value mean off.
func isZeroSetting(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	for _, r := range v {
		if r != '0' {
			return false
		}
	}
	return true
}
