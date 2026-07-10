/* anylmdb lmdb.h shim.
 *
 * Two roles:
 *  - For the vendored engine sources and the per-stream glue TUs (which
 *    define ANYLMDB_STREAM), selects the matching pristine upstream header.
 *  - For consumers of anylmdb (no ANYLMDB_STREAM), presents the LMDB 1.0
 *    header as the public API surface. anylmdb exports the full 1.0 mdb_*
 *    surface; 1.0-only functions return ENOTSUP on a 0.9-format environment.
 */
#ifndef ANYLMDB_LMDB_H_SHIM
#define ANYLMDB_LMDB_H_SHIM

#ifdef MDB_VL32
# error "anylmdb does not support MDB_VL32 builds"
#endif

#if !defined(ANYLMDB_STREAM)
# include "lmdb_lmdb10.h"
#elif ANYLMDB_STREAM == 9
# include "lmdb_lmdb09.h"
#elif ANYLMDB_STREAM == 10
# include "lmdb_lmdb10.h"
#else
# error "invalid ANYLMDB_STREAM (must be 9 or 10)"
#endif

#endif /* ANYLMDB_LMDB_H_SHIM */
