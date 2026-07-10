/* Compiles the pristine LMDB 0.9 engine (mdb.c) with every extern symbol
 * renamed to mdb09_*. The upstream source is vendored byte-identical as
 * mdb_lmdb09.c.h; all adaptation happens here. */
#define ANYLMDB_STREAM 9
#include "anylmdb_rename09.h"
#include "mdb_lmdb09.c.h"
