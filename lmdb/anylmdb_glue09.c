/* Trampolines and dispatch table for the LMDB 0.9 engine. This TU sees only
 * the 0.9 stream's types and (renamed) symbols; the mdb_* names below expand
 * to mdb09_* via the rename header. */
#define ANYLMDB_STREAM 9
#include "anylmdb_rename09.h"
#include "lmdb.h"
#include "anylmdb_ops.h"

_Static_assert(sizeof(size_t) == 8, "anylmdb requires a 64-bit build");
_Static_assert(sizeof(MDB_val) == 2 * sizeof(void *), "unexpected MDB_val layout");

#define ANYLMDB_T(ret, name, params, args) \
    static ret t_##name params { return mdb_##name args; }
ANYLMDB_OPS_COMMON(ANYLMDB_T)
#undef ANYLMDB_T

#define ANYLMDB_TV(ret, name, params, args) \
    static void t_##name params { mdb_##name args; }
ANYLMDB_OPS_COMMON_VOID(ANYLMDB_TV)
#undef ANYLMDB_TV

/* The engine calls this with its own MDB_env; the wrapper env is recovered
 * from the engine userctx, which the core always points back at the wrapper. */
static void t_assert_cb(MDB_env *env, const char *msg)
{
    anylmdb_dispatch_assert(mdb_env_get_userctx(env), msg);
}

static int t_set_assert(void *env, int install)
{
    return mdb_env_set_assert((MDB_env *)env, install ? t_assert_cb : NULL);
}

const anylmdb_ops anylmdb_ops09 = {
#define ANYLMDB_I(ret, name, params, args) .name = t_##name,
    ANYLMDB_OPS_COMMON(ANYLMDB_I)
    ANYLMDB_OPS_COMMON_VOID(ANYLMDB_I)
#undef ANYLMDB_I
    /* ANYLMDB_OPS_V10 slots stay NULL: that is the ENOTSUP policy. */
    .set_assert = t_set_assert,
};
