// Package certstore owns every write of the agent's client key, client
// certificate and CA after registration. It adopts a renewed certificate in
// stages, so that a crash at any point leaves a matching key and
// certificate: the full child or the full parent.
//
// "Parent" is the certificate that the agent holds before a renewal.
// "Child" is the certificate that Nodeward returns. Nodeward shortens the
// parent before it returns the child, so a child that is on disk and passes
// the local checks is never discarded.
//
// The package mints nothing and calls no network. Prepare, Adopt and
// Recover do not take the renewal lock: the caller holds it
// (AcquireRenewalLock).
package certstore

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/runos-official/nodeagent/roslog"
)

// Paths names the three live credential files.
type Paths struct {
	Cert string
	Key  string
	CA   string
}

// Material is a key, certificate and CA as PEM bytes.
type Material struct {
	KeyPEM  []byte
	CertPEM []byte
	CAPEM   []byte
}

// Errors of the store.
var (
	// ErrChildRejected: a staged child failed a local check. The parent
	// stays. RejectReason gives the reason word.
	ErrChildRejected = errors.New("certstore: renewed certificate rejected")
	// ErrRecoveredToParent: a staged child file was damaged after the commit
	// point. Recover restored the full parent.
	ErrRecoveredToParent = errors.New("certstore: staged certificate damaged after commit; restored the held certificate")
	// ErrAdoptionSuperseded: another writer (a new registration) replaced the
	// live files during an adoption. The live files stay untouched.
	ErrAdoptionSuperseded = errors.New("certstore: live credentials were replaced by another writer; adoption abandoned")
	// ErrStoreDamaged: the store cannot reach a matching key and certificate
	// by itself. Recover changes nothing more.
	ErrStoreDamaged = errors.New("certstore: credential store damaged")
	// ErrNotPrepared: Adopt runs only after Prepare.
	ErrNotPrepared = errors.New("certstore: adopt without prepare")
	// ErrRecoverFirst: Prepare found a committed or staged adoption.
	ErrRecoverFirst = errors.New("certstore: an earlier adoption is not finished; run Recover first")
)

// CrashPoints lists every named point of Prepare, Adopt and Recover, in
// order. A test hook can stop the store at each point.
var CrashPoints = []string{
	// Prepare.
	"prev-ca", "prev-key", "prev-cert", "prepared",
	// Adopt, before a complete staged child is durable.
	"staged-ca", "staged-key",
	// Adopt, from the durable staged child on.
	"staged-cert", "checked", "committed",
	"renamed-ca", "renamed-key", "renamed-cert", "dir-synced", "adopted", "prev-removed",
	// Recover, roll forward.
	"recover-committed", "recover-renamed-ca", "recover-renamed-key", "recover-renamed-cert",
	"recover-dir-synced", "recover-adopted", "recover-prev-removed",
	// Recover, restore of the parent after damage.
	"recover-restored-ca", "recover-restored-key", "recover-restored-cert",
}

const (
	nextSuffix = ".next"
	prevSuffix = ".prev"
	keyMode    = fs.FileMode(0o600)
	certMode   = fs.FileMode(0o644)
)

// Store adopts renewed credentials into the files of Paths.
type Store struct {
	paths Paths
	now   func() time.Time
	// hook runs at each crash point. A non-nil result stops the function
	// there and is returned. Tests only.
	hook func(point string) error
	// afterTemp runs after each temporary file is synced. Tests only.
	afterTemp func(path string)
}

// Open returns a store for p. Open does no I/O.
func Open(p Paths) *Store {
	return &Store{paths: p, now: time.Now}
}

func (s *Store) at(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

// credFile is one credential file. files() gives them in the fixed order:
// CA, key, certificate. The certificate is always last.
type credFile struct {
	name string
	live string
	key  bool
}

func (s *Store) files() []credFile {
	return []credFile{
		{name: "ca", live: s.paths.CA},
		{name: "key", live: s.paths.Key, key: true},
		{name: "cert", live: s.paths.Cert},
	}
}

func (d Digests) get(name string) string {
	switch name {
	case "ca":
		return d.CA
	case "key":
		return d.Key
	}
	return d.Cert
}

func (d *Digests) set(name, v string) {
	switch name {
	case "ca":
		d.CA = v
	case "key":
		d.Key = v
	default:
		d.Cert = v
	}
}

func (m Material) get(name string) []byte {
	switch name {
	case "ca":
		return m.CAPEM
	case "key":
		return m.KeyPEM
	}
	return m.CertPEM
}

func (m *Material) set(name string, b []byte) {
	switch name {
	case "ca":
		m.CAPEM = b
	case "key":
		m.KeyPEM = b
	default:
		m.CertPEM = b
	}
}

func (s *Store) journalPath() string {
	return filepath.Join(dirOf(s.paths.Cert), JournalName)
}

// syncDirs syncs every directory that holds a credential file or the
// journal.
func (s *Store) syncDirs() error {
	var dirs []string
	for _, f := range s.files() {
		if d := dirOf(f.live); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

// readFiles reads the three files with the given suffix.
func (s *Store) readFiles(suffix string) (Material, error) {
	var m Material
	for _, f := range s.files() {
		b, err := os.ReadFile(f.live + suffix)
		if err != nil {
			return Material{}, err
		}
		m.set(f.name, b)
	}
	return m, nil
}

func digestsOf(m Material) Digests {
	return Digests{CA: digest(m.CAPEM), Key: digest(m.KeyPEM), Cert: digest(m.CertPEM)}
}

// liveDigests returns the digest of each live file; "" for a missing file.
func (s *Store) liveDigests() (Digests, error) {
	var d Digests
	for _, f := range s.files() {
		v, err := fileDigest(f.live)
		if err != nil {
			return Digests{}, err
		}
		d.set(f.name, v)
	}
	return d, nil
}

// strayPaths lists the staged, previous and temporary files that exist.
// It reads no credential file.
func (s *Store) strayPaths() []string {
	var out []string
	for _, f := range s.files() {
		for _, p := range []string{
			f.live + nextSuffix, f.live + prevSuffix,
			f.live + nextSuffix + tmpSuffix, f.live + prevSuffix + tmpSuffix,
		} {
			if exists(p) {
				out = append(out, p)
			}
		}
	}
	if p := s.journalPath() + tmpSuffix; exists(p) {
		out = append(out, p)
	}
	return out
}

// removeAll removes paths and syncs the directories.
func (s *Store) removeAll(paths []string) error {
	for _, p := range paths {
		if err := removeIfPresent(p); err != nil {
			return err
		}
	}
	return s.syncDirs()
}

// Adopted returns the journal record. ok is false when there is no
// journal.
func (s *Store) Adopted() (rec Record, ok bool, err error) {
	return readJournal(s.journalPath())
}

// HeldSerial returns the serial of the live certificate as lower-case
// hexadecimal text.
func (s *Store) HeldSerial() (string, error) {
	b, err := os.ReadFile(s.paths.Cert)
	if err != nil {
		return "", err
	}
	c, err := parseOneCert(b)
	if err != nil {
		return "", fmt.Errorf("parse held certificate: %w", err)
	}
	return serialText(c), nil
}

// Prepare copies the live parent to the .prev files and writes the journal
// in state prepared. Call Prepare under the renewal lock, before the mint.
// Prepare refuses, and writes nothing, when the live key pair is not usable
// or an earlier adoption is not finished.
func (s *Store) Prepare() error {
	rec, ok, err := readJournal(s.journalPath())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStoreDamaged, err)
	}
	if ok && rec.State == StateCommitting {
		return ErrRecoverFirst
	}
	for _, f := range s.files() {
		if exists(f.live + nextSuffix) {
			return ErrRecoverFirst
		}
	}
	live, err := s.readFiles("")
	if err != nil {
		return fmt.Errorf("read held credentials: %w", err)
	}
	if _, err := tls.X509KeyPair(live.CertPEM, live.KeyPEM); err != nil {
		return fmt.Errorf("held key pair is not usable: %w", err)
	}
	parent, err := parseOneCert(live.CertPEM)
	if err != nil {
		return fmt.Errorf("parse held certificate: %w", err)
	}

	var prev Digests
	for _, f := range s.files() {
		// A key copy is always 0600, whatever the mode of the live key.
		mode := keyMode
		if !f.key {
			info, err := os.Stat(f.live)
			if err != nil {
				return err
			}
			mode = info.Mode().Perm()
		}
		data := live.get(f.name)
		if err := writeFileSync(f.live+prevSuffix, data, mode, s.afterTemp); err != nil {
			return err
		}
		prev.set(f.name, digest(data))
		if err := s.at("prev-" + f.name); err != nil {
			return err
		}
	}
	next := Record{
		State:        StatePrepared,
		Sequence:     rec.Sequence,
		Prev:         prev,
		ParentSerial: serialText(parent),
		UpdatedAt:    s.now().UTC(),
	}
	if err := writeJournal(s.journalPath(), next, s.afterTemp); err != nil {
		return err
	}
	if err := s.syncDirs(); err != nil {
		return err
	}
	return s.at("prepared")
}

// Adopt stages the child m, checks it against the live parent, commits it
// and renames it into place. A rejected child leaves the parent and returns
// an error that matches ErrChildRejected. A failure after the commit point
// leaves the journal in state committing; the next Recover rolls forward.
func (s *Store) Adopt(m Material) error {
	rec, ok, err := readJournal(s.journalPath())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStoreDamaged, err)
	}
	if !ok || rec.State != StatePrepared {
		return ErrNotPrepared
	}
	live, err := s.readFiles("")
	if err != nil {
		return fmt.Errorf("read held credentials: %w", err)
	}
	if digestsOf(live) != rec.Prev {
		return ErrAdoptionSuperseded
	}
	parent, err := parseOneCert(live.CertPEM)
	if err != nil {
		return fmt.Errorf("parse held certificate: %w", err)
	}

	// Stage: CA, key, certificate. Each .next file is complete when it
	// appears, because it is renamed from a synced temporary file.
	for _, f := range s.files() {
		mode := certMode
		if f.key {
			mode = keyMode
		}
		if err := writeFileSync(f.live+nextSuffix, m.get(f.name), mode, s.afterTemp); err != nil {
			return err
		}
		if f.name == "cert" {
			break
		}
		if err := s.at("staged-" + f.name); err != nil {
			return err
		}
	}
	// The staged child is durable before the checks run.
	if err := s.syncDirs(); err != nil {
		return err
	}
	if err := s.at("staged-cert"); err != nil {
		return err
	}

	staged, err := s.readFiles(nextSuffix)
	if err != nil {
		return fmt.Errorf("read staged certificate: %w", err)
	}
	child, err := checkChild(staged, parent, s.now())
	if err != nil {
		roslog.W("Rejected the renewed client certificate; keeping the held certificate", nil,
			"reason", RejectReason(err), "held_serial", serialText(parent))
		if rerr := s.removeAll(s.nextPaths()); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	if err := s.at("checked"); err != nil {
		return err
	}
	// THE COMMIT POINT: from here a crash rolls forward to the child.
	committing := Record{
		State:         StateCommitting,
		Sequence:      rec.Sequence,
		Prev:          rec.Prev,
		Next:          digestsOf(staged),
		ParentSerial:  rec.ParentSerial,
		ChildSerial:   serialText(child),
		ChildNotAfter: child.NotAfter.UTC(),
		UpdatedAt:     s.now().UTC(),
	}
	if err := writeJournal(s.journalPath(), committing, s.afterTemp); err != nil {
		return err
	}
	if err := s.at("committed"); err != nil {
		return err
	}
	return s.rollForward(committing, "")
}

func (s *Store) nextPaths() []string {
	var out []string
	for _, f := range s.files() {
		out = append(out, f.live+nextSuffix, f.live+nextSuffix+tmpSuffix)
	}
	return out
}

func (s *Store) prevPaths() []string {
	var out []string
	for _, f := range s.files() {
		out = append(out, f.live+prevSuffix, f.live+prevSuffix+tmpSuffix)
	}
	return out
}

// rollForward renames the staged child into place in the fixed order,
// writes state adopted and removes the parent copies. prefix names the
// crash points ("" for Adopt, "recover-" for Recover).
func (s *Store) rollForward(rec Record, prefix string) error {
	for _, f := range s.files() {
		if next := f.live + nextSuffix; exists(next) {
			if err := os.Rename(next, f.live); err != nil {
				return fmt.Errorf("rename %s: %w", next, err)
			}
		}
		if err := s.at(prefix + "renamed-" + f.name); err != nil {
			return err
		}
	}
	if err := s.syncDirs(); err != nil {
		return err
	}
	if err := s.at(prefix + "dir-synced"); err != nil {
		return err
	}
	now := s.now().UTC()
	adopted := Record{
		State:     StateAdopted,
		Sequence:  rec.Sequence + 1,
		Serial:    rec.ChildSerial,
		NotAfter:  rec.ChildNotAfter,
		AdoptedAt: now,
		UpdatedAt: now,
	}
	if err := writeJournal(s.journalPath(), adopted, s.afterTemp); err != nil {
		return err
	}
	if err := s.at(prefix + "adopted"); err != nil {
		return err
	}
	// The parent key does not stay on disk.
	if err := s.removeAll(append(s.prevPaths(), s.nextPaths()...)); err != nil {
		return err
	}
	if err := s.at(prefix + "prev-removed"); err != nil {
		return err
	}
	roslog.I("Adopted the renewed client certificate",
		"serial", adopted.Serial, "not_after", adopted.NotAfter.Format(time.RFC3339),
		"previous_serial", rec.ParentSerial, "sequence", adopted.Sequence)
	return nil
}

// Recover brings the store to rest after a crash. It is idempotent. With no
// journal and no staged, previous or temporary file it changes nothing,
// writes no log line and reads no credential file.
//
// It returns nil when the live files are a matching pair at rest,
// ErrRecoveredToParent after a restore, ErrAdoptionSuperseded when another
// writer replaced the live files, and ErrStoreDamaged when it cannot reach
// a matching pair.
func (s *Store) Recover() error {
	rec, ok, err := readJournal(s.journalPath())
	if err != nil {
		roslog.E("Certificate store journal is unreadable; changing nothing", err)
		return fmt.Errorf("%w: %v", ErrStoreDamaged, err)
	}
	stray := s.strayPaths()

	// Rules 1 and 2: at rest.
	if !ok || rec.State == StateAdopted {
		if len(stray) == 0 {
			return nil
		}
		if err := s.removeAll(stray); err != nil {
			return err
		}
		roslog.I("Removed leftover certificate store files", "count", len(stray))
		return s.checkLive(true)
	}

	liveD, err := s.liveDigests()
	if err != nil {
		return err
	}
	// Rule 3: another writer replaced the live files.
	if s.superseded(rec, liveD) {
		if err := s.removeAll(append(s.prevPaths(), s.nextPaths()...)); err != nil {
			return err
		}
		serial, _ := s.HeldSerial()
		if err := writeJournal(s.journalPath(), Record{
			State: StateAdopted, Sequence: rec.Sequence, Serial: serial, UpdatedAt: s.now().UTC(),
		}, s.afterTemp); err != nil {
			return err
		}
		roslog.W("Live credentials were replaced during a certificate adoption; keeping them", nil,
			"state", rec.State, "held_serial", serial)
		return ErrAdoptionSuperseded
	}

	if rec.State == StatePrepared {
		committing, ok := s.validStage(rec)
		if !ok {
			// Rule 5: no usable child; the parent stays.
			if err := s.removeAll(append(s.prevPaths(), s.nextPaths()...)); err != nil {
				return err
			}
			if err := s.writeAtRest(rec.Sequence); err != nil {
				return err
			}
			roslog.I("Discarded an unfinished certificate adoption; keeping the held certificate",
				"held_serial", rec.ParentSerial)
			return s.checkLive(false)
		}
		// Rule 4: the crash fell between the checks and the commit write.
		// The child wins.
		if err := writeJournal(s.journalPath(), committing, s.afterTemp); err != nil {
			return err
		}
		if err := s.at("recover-committed"); err != nil {
			return err
		}
		rec = committing
	}

	// Rules 6 and 7: state committing.
	if s.stageIntact(rec, liveD) {
		if err := s.rollForward(rec, "recover-"); err != nil {
			return err
		}
		return s.checkLive(false)
	}
	return s.restoreParent(rec, liveD)
}

// superseded reports whether a live file has neither the parent digest nor,
// in state committing, the child digest.
func (s *Store) superseded(rec Record, liveD Digests) bool {
	for _, f := range s.files() {
		d := liveD.get(f.name)
		if d != "" && d == rec.Prev.get(f.name) {
			continue
		}
		if rec.State == StateCommitting && d != "" && d == rec.Next.get(f.name) {
			continue
		}
		return true
	}
	return false
}

// validStage returns the committing record when three staged files exist
// and pass the local checks against the live parent.
func (s *Store) validStage(rec Record) (Record, bool) {
	staged, err := s.readFiles(nextSuffix)
	if err != nil {
		return Record{}, false
	}
	parentPEM, err := os.ReadFile(s.paths.Cert)
	if err != nil {
		return Record{}, false
	}
	parent, err := parseOneCert(parentPEM)
	if err != nil {
		return Record{}, false
	}
	child, err := checkChild(staged, parent, s.now())
	if err != nil {
		return Record{}, false
	}
	return Record{
		State:         StateCommitting,
		Sequence:      rec.Sequence,
		Prev:          rec.Prev,
		Next:          digestsOf(staged),
		ParentSerial:  rec.ParentSerial,
		ChildSerial:   serialText(child),
		ChildNotAfter: child.NotAfter.UTC(),
		UpdatedAt:     s.now().UTC(),
	}, true
}

// stageIntact reports whether every file of the child is either staged with
// its journal digest or already live with that digest.
func (s *Store) stageIntact(rec Record, liveD Digests) bool {
	return s.intact(rec.Next, nextSuffix, liveD)
}

func (s *Store) intact(want Digests, suffix string, liveD Digests) bool {
	for _, f := range s.files() {
		w := want.get(f.name)
		if w == "" {
			return false
		}
		p := f.live + suffix
		if exists(p) {
			d, err := fileDigest(p)
			if err != nil || d != w {
				return false
			}
			continue
		}
		if liveD.get(f.name) != w {
			return false
		}
	}
	return true
}

// restoreParent is rule 7: a staged file is damaged after the commit.
// It restores the full parent from the .prev files, or changes nothing and
// returns ErrStoreDamaged when the .prev files are damaged too.
func (s *Store) restoreParent(rec Record, liveD Digests) error {
	if !s.intact(rec.Prev, prevSuffix, liveD) {
		roslog.E("Certificate store is damaged: the staged and the previous credentials both fail their digests", nil,
			"parent_serial", rec.ParentSerial, "child_serial", rec.ChildSerial)
		return ErrStoreDamaged
	}
	for _, f := range s.files() {
		if prev := f.live + prevSuffix; exists(prev) {
			if err := os.Rename(prev, f.live); err != nil {
				return fmt.Errorf("restore %s: %w", prev, err)
			}
		}
		if err := s.at("recover-restored-" + f.name); err != nil {
			return err
		}
	}
	if err := s.removeAll(append(s.nextPaths(), s.prevPaths()...)); err != nil {
		return err
	}
	if err := s.writeAtRest(rec.Sequence); err != nil {
		return err
	}
	roslog.W("A staged certificate was damaged after commit; restored the held certificate", nil,
		"held_serial", rec.ParentSerial, "child_serial", rec.ChildSerial)
	if err := s.checkLive(false); err != nil {
		return err
	}
	return ErrRecoveredToParent
}

// writeAtRest writes state adopted for the live certificate and keeps the
// sequence number.
func (s *Store) writeAtRest(seq uint64) error {
	rec := Record{State: StateAdopted, Sequence: seq, UpdatedAt: s.now().UTC()}
	if b, err := os.ReadFile(s.paths.Cert); err == nil {
		if c, err := parseOneCert(b); err == nil {
			rec.Serial = serialText(c)
			rec.NotAfter = c.NotAfter.UTC()
		}
	}
	return writeJournal(s.journalPath(), rec, s.afterTemp)
}

// checkLive verifies that the live key matches the live certificate. With
// allowEmpty, a missing or empty certificate (an unregistered node) passes.
func (s *Store) checkLive(allowEmpty bool) error {
	certPEM, cerr := os.ReadFile(s.paths.Cert)
	if allowEmpty && (errors.Is(cerr, fs.ErrNotExist) || (cerr == nil && len(certPEM) == 0)) {
		return nil
	}
	keyPEM, kerr := os.ReadFile(s.paths.Key)
	if err := errors.Join(cerr, kerr); err != nil {
		return fmt.Errorf("%w: %v", ErrStoreDamaged, err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		roslog.E("Live client key does not match the live certificate", err)
		return fmt.Errorf("%w: %v", ErrStoreDamaged, err)
	}
	return nil
}
