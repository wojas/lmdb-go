/* anylmdb — one library carrying both LMDB 0.9 and LMDB 1.0.
 *
 * anylmdb exports the standard mdb_* API (LMDB 1.0 surface, see lmdb.h) and
 * picks the matching engine per environment: existing data files are
 * detected by their on-disk format at mdb_env_open(); new databases use the
 * configured default format (LMDB 0.9 unless overridden). This header adds
 * the small anylmdb-specific control/introspection API.
 */
#ifndef ANYLMDB_H
#define ANYLMDB_H

#include "lmdb.h"

#ifdef __cplusplus
extern "C" {
#endif

#define ANYLMDB_VERSION_MAJOR 0
#define ANYLMDB_VERSION_MINOR 1
#define ANYLMDB_VERSION_PATCH 0
#define ANYLMDB_VERSION_STRING "0.1.0"

/** Engine / data-format selector. */
typedef enum anylmdb_ver {
    ANYLMDB_VER_NEW = -1,    /**< sniff result: data file absent or empty */
    ANYLMDB_VER_DEFAULT = 0, /**< use the process-wide default */
    ANYLMDB_VER_09 = 9,      /**< LMDB 0.9 engine / data format */
    ANYLMDB_VER_10 = 10,     /**< LMDB 1.0 engine / data format */
} anylmdb_ver;

/** The data file has mm_version 2: written by lmdb-js prerelease builds
 * (a pre-1.0 development snapshot), incompatible with both 0.9 and 1.0. */
#define ANYLMDB_FORMAT_UNSUPPORTED (-30599)

/** Set the process-wide default format for newly created databases.
 * Accepts ANYLMDB_VER_09, ANYLMDB_VER_10, or ANYLMDB_VER_DEFAULT (reset to
 * built-in behavior: the ANYLMDB_DEFAULT environment variable — "09" or
 * "10" — if set, else LMDB 0.9). Existing data files are always opened
 * with the engine matching their on-disk format. */
int anylmdb_set_default_version(anylmdb_ver ver);

/** Choose the format for THIS environment if mdb_env_open() creates a new
 * database. Must be called before mdb_env_open(); returns EINVAL after.
 * Ignored when the data file already exists (its format wins). */
int anylmdb_env_set_version(MDB_env *env, anylmdb_ver ver);

/** After mdb_env_open(): the engine actually in use (ANYLMDB_VER_09/10).
 * Before open: the value set with anylmdb_env_set_version(), or
 * ANYLMDB_VER_DEFAULT. */
int anylmdb_env_get_version(MDB_env *env, anylmdb_ver *ver);

/** Detect the format of an environment without opening it (reads the data
 * file's meta page; never touches the lock file). `flags` is checked for
 * MDB_NOSUBDIR only. Returns 0 and sets *ver to ANYLMDB_VER_09,
 * ANYLMDB_VER_10 or ANYLMDB_VER_NEW; or an error: ANYLMDB_FORMAT_UNSUPPORTED,
 * MDB_INVALID (not an LMDB file), MDB_VERSION_MISMATCH (LMDB file with an
 * unknown mm_version), or an errno from open/read. */
int anylmdb_sniff(const char *path, unsigned int flags, anylmdb_ver *ver);

#ifdef __cplusplus
}
#endif

#endif /* ANYLMDB_H */
