/* Compiles the pristine LMDB 0.9 midl.c with every extern symbol renamed
 * to mdb09_*. See anylmdb_mdb09.c. */
#define ANYLMDB_STREAM 9
#include "anylmdb_rename09.h"
#include "midl_lmdb09.c.h"
