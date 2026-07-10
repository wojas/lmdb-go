/* Compiles the pristine LMDB 1.0 engine (mdb.c) with every extern symbol
 * renamed to mdb10_*. The upstream source is vendored byte-identical as
 * mdb_lmdb10.c.h; all adaptation happens here. 1.0's module.c/crypto.c are
 * deliberately not built (no dlopen dependency); the wrapper provides
 * mdb_modload/mdb_modunload/mdb_modsetup replacements. */
#define ANYLMDB_STREAM 10
#include "anylmdb_rename10.h"
#include "mdb_lmdb10.c.h"
