// The data formats of LMDB 0.9 and 1.0, with lmdb-go v2's API for them
// (lmdb-go v2 lmdb/version.go: Format, V09, V10, SetDefaultFormat,
// Env.SetFormat, Env.Format, Env.EngineVersion). In lmdb-go v2 each
// format has its own C engine; here one engine (internal/engine, ported
// from rlmdb) reads and writes both (rlmdb DUAL_FORMAT.md).

package lmdb

import (
	"errors"
	"syscall"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// Format identifies one of the two on-disk formats of LMDB.
//
// Each Env uses exactly one format, selected when Open is called: an existing
// database is always opened in its on-disk format (detected by SniffFormat),
// while a new database uses the explicitly requested format (Env.SetFormat)
// or the process default (SetDefaultFormat, DefaultFormatEnvVar, or V09).
type Format int

const (
	// FormatUnknown means no LMDB format has been determined (yet).
	FormatUnknown Format = 0
	// V09 is the format of LMDB 0.9.x (on-disk data format 1).
	V09 Format = 9
	// V10 is the format of LMDB 1.0.x (on-disk data format 3).
	// The two formats are mutually incompatible on disk.
	V10 Format = 10
)

// PrevSnapshot is the Env.Open flag of LMDB 1.0 (and lmdb-go v2) that opens
// the environment with the previous snapshot rather than the latest one.
// It is not implemented, as in rlmdb: Open fails with EINVAL for either
// format.
const PrevSnapshot = 0x2000000

func (v Format) valid() bool { return v == V09 || v == V10 }

// String returns "0.9", "1.0" or "unknown".
func (v Format) String() string {
	switch v {
	case V09:
		return "0.9"
	case V10:
		return "1.0"
	default:
		return "unknown"
	}
}

// DefaultFormatEnvVar is the environment variable consulted for the
// process-wide default format when SetDefaultFormat has not been called.
// Accepted values: "09", "9", "0.9", "10", "1.0"; others are ignored.  It is
// read whenever a new database is created.
//
// The variable is experimental, as in lmdb-go v2 and rlmdb (whose
// RLMDB_DEFAULT_FORMAT it mirrors): a stray setting silently creates LMDB 1.0
// databases that LMDB 0.9 programs cannot open.  Programs should prefer
// Env.SetFormat or SetDefaultFormat.
const DefaultFormatEnvVar = engine.DefaultFormatEnv

// ErrInvalidFormat is returned when a Format value is not V09 or V10.
var ErrInvalidFormat = errors.New("lmdb: invalid LMDB format")

// ErrVersionUndetermined is returned by Env.EngineVersion before the format
// of the environment is known (no explicit request and not yet opened).
var ErrVersionUndetermined = errors.New("lmdb: LMDB version not determined yet")

// ErrFormatConflict is returned by Env.Open when the on-disk format of an
// existing database differs from the format explicitly requested with
// Env.SetFormat. lmdb-go never converts between formats; migrating
// requires a dump and reload (which can be done in a single process by
// opening a source and a destination Env of different formats).
var ErrFormatConflict = errors.New("lmdb: existing database format does not match the explicitly requested format")

// SetDefaultFormat sets the process-wide default format used when Env.Open
// creates a new database and no format was requested on the Env.
// Passing FormatUnknown reverts to the built-in behavior (the
// DefaultFormatEnvVar environment variable, or V09).
//
// The default is V09: while LMDB 0.9 remains what distributions and other
// tools ship, databases created in the 0.9 format stay readable by them.
func SetDefaultFormat(v Format) error {
	switch v {
	case FormatUnknown, V09, V10:
	default:
		return ErrInvalidFormat
	}
	engine.SetDefaultFormat(format.Format(v))
	return nil
}

// defaultFormat is the format of new databases that do not ask for one:
// SetDefaultFormat wins over DefaultFormatEnvVar, which wins over V09.
func defaultFormat() Format { return Format(engine.DefaultFormat()) }

// EnvVersion describes the LMDB release whose format and behaviour an Env
// matches.
type EnvVersion struct {
	Major   int
	Minor   int
	Patch   int
	Release string // human-readable release string
}

// String returns the release string.
func (v EnvVersion) String() string { return v.Release }

// The C releases whose data format and behaviour this implementation
// matches, per format (rlmdb's version_info, v09 and v10).
var (
	envVersion09 = EnvVersion{0, 9, 36, "LMDB 0.9.36 format (" + versionString + ")"}
	envVersion10 = EnvVersion{1, 0, 2, "LMDB 1.0.2 format (" + versionString + ")"}
)

// SetFormat requests the on-disk format used if Open creates a NEW
// database. It must be called before Open.
//
// The request only applies to database creation: an existing database is
// always opened in its on-disk format, and if that format differs from an
// explicitly requested format, Open fails with ErrFormatConflict rather than
// converting anything.
//
// Passing FormatUnknown clears the request.
func (env *Env) SetFormat(v Format) error {
	switch v {
	case FormatUnknown, V09, V10:
	default:
		return ErrInvalidFormat
	}

	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if env.opened || env.closed {
		return errors.New("lmdb: SetFormat must be called before Open")
	}
	env.reqFormat = v
	return nil
}

// Format returns the on-disk format of this environment. Before Open it
// returns the format requested with SetFormat, or FormatUnknown.
func (env *Env) Format() Format {
	env.closeLock.RLock()
	defer env.closeLock.RUnlock()
	if e := env.eng.Load(); e != nil {
		return Format(e.Format())
	}
	return env.reqFormat
}

// EngineVersion returns the LMDB release whose data format and behaviour
// this environment matches: 0.9.36 for an LMDB 0.9 environment, 1.0.2 for an
// LMDB 1.0 one (the releases rlmdb follows).  Before Open it reports the
// explicitly requested format, if any; otherwise it returns
// ErrVersionUndetermined.
//
// The package-level Version and VersionString report the implementation
// (0.9.90, above every C release); EngineVersion is the per-environment
// format.
func (env *Env) EngineVersion() (EnvVersion, error) {
	switch env.Format() {
	case V09:
		return envVersion09, nil
	case V10:
		return envVersion10, nil
	}
	return EnvVersion{}, ErrVersionUndetermined
}

// SetPageSize sets the page size of a new LMDB 1.0 environment: a power of
// two from 256 to 65536 (0 restores the default, the OS page size).  It must
// be called before Open.  An existing environment keeps its page size; Open
// fails with EINVAL if the environment turns out to be an LMDB 0.9 one,
// which has no such setting.
//
// See mdb_env_set_pagesize (LMDB 1.0) and rlmdb_env_set_pagesize.
func (env *Env) SetPageSize(size int) error {
	if size != 0 && (size&(size-1) != 0 || size < format.MinPageSize || size > format.LayoutV10.MaxPageSize) {
		return &OpError{Op: "mdb_env_set_pagesize", Errno: syscall.EINVAL}
	}
	env.closeLock.Lock()
	defer env.closeLock.Unlock()
	if env.closed {
		return errClosed
	}
	if env.opened {
		return &OpError{Op: "mdb_env_set_pagesize", Errno: syscall.EINVAL}
	}
	env.opts.PageSize = size
	return nil
}
