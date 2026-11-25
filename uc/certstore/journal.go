package certstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// JournalName is the file name of the adoption journal, in the directory of
// the certificate file.
const JournalName = "mtls.adoption.json"

// Journal states.
const (
	// StatePrepared: the parent is copied to the .prev files. No child is
	// committed.
	StatePrepared = "prepared"
	// StateCommitting: a staged child passed the local checks. From here
	// recovery rolls forward.
	StateCommitting = "committing"
	// StateAdopted: the store is at rest. Serial names the live certificate.
	StateAdopted = "adopted"
)

// Digests holds the SHA-256 of the three credential files.
type Digests struct {
	CA   string `json:"ca,omitempty"`
	Key  string `json:"key,omitempty"`
	Cert string `json:"cert,omitempty"`
}

// Record is the journal. It holds digests, serials, times and a sequence
// number, never key material.
type Record struct {
	State string `json:"state"`
	// Sequence rises by 1 each time a child becomes the live certificate.
	Sequence uint64 `json:"sequence"`
	// Prev holds the digests of the .prev copies of the parent.
	Prev Digests `json:"prev,omitzero"`
	// Next holds the digests of the staged child (state committing).
	Next          Digests   `json:"next,omitzero"`
	ParentSerial  string    `json:"parent_serial,omitempty"`
	ChildSerial   string    `json:"child_serial,omitempty"`
	ChildNotAfter time.Time `json:"child_not_after,omitzero"`
	// Serial and NotAfter name the live certificate in state adopted.
	Serial    string    `json:"serial,omitempty"`
	NotAfter  time.Time `json:"not_after,omitzero"`
	AdoptedAt time.Time `json:"adopted_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// journalMode is the mode of the journal file. The journal holds no secret.
const journalMode fs.FileMode = 0o644

// readJournal reads the journal. ok is false when the file does not exist.
// A file that does not parse, or names an unknown state, is an error.
func readJournal(path string) (rec Record, ok bool, err error) {
	b, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read journal %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, false, fmt.Errorf("%w: parse journal %s: %v", ErrStoreDamaged, path, err)
	}
	switch rec.State {
	case StatePrepared, StateCommitting, StateAdopted:
	default:
		return Record{}, false, fmt.Errorf("%w: journal %s has unknown state %q", ErrStoreDamaged, path, rec.State)
	}
	return rec, true, nil
}

// writeJournal replaces the journal atomically and syncs its directory.
func writeJournal(path string, rec Record, afterTemp func(string)) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode journal: %w", err)
	}
	if err := writeFileSync(path, append(b, '\n'), journalMode, afterTemp); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	return syncDir(dirOf(path))
}
