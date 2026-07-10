/* anylmdb midl.h shim: selects the pristine upstream midl.h matching
 * ANYLMDB_STREAM. Only the vendored engine sources include this.
 */
#ifndef ANYLMDB_MIDL_H_SHIM
#define ANYLMDB_MIDL_H_SHIM

#if !defined(ANYLMDB_STREAM)
# error "midl.h is internal to the anylmdb engine builds (ANYLMDB_STREAM not set)"
#elif ANYLMDB_STREAM == 9
# include "midl_lmdb09.h"
#elif ANYLMDB_STREAM == 10
# include "midl_lmdb10.h"
#else
# error "invalid ANYLMDB_STREAM (must be 9 or 10)"
#endif

#endif /* ANYLMDB_MIDL_H_SHIM */
