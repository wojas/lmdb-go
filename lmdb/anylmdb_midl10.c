/* Compiles the pristine LMDB 1.0 midl.c with every extern symbol renamed
 * to mdb10_*. See anylmdb_mdb10.c. */
#define ANYLMDB_STREAM 10
#include "anylmdb_rename10.h"
#include "midl_lmdb10.c.h"
