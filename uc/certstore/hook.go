package certstore

// SetCrashHook makes the store call h at each point of CrashPoints. A
// non-nil result stops the function at that point and is returned, as a
// crash would stop it. Tests of callers use it to build each crash state
// through the real store. Production code never calls it.
func (s *Store) SetCrashHook(h func(point string) error) { s.hook = h }
