// Package patchfacts collects the node patch facts record from the node it runs
// on: the running and installed kernels, whether a reboot is pending and which
// packages asked for it, how many security updates wait and which, how old the
// package metadata behind that count is, when the last package run succeeded,
// when the node booted, what state the automatic update units are in, and which
// packages are held.
//
// The package produces one record and returns it. It sends nothing and it
// schedules nothing: carriage and the slow-pass interval belong elsewhere.
//
// Every command the collector can run is a row of the table in commands.go, and
// the collector runs nothing that is not in that table. Exactly one row writes
// anything, the package metadata refresh, and that row runs only on a slow pass
// and only when no package manager lock is held. Every other row, and every
// file the collector reads, leaves the node unchanged.
package patchfacts
