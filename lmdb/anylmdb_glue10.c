/* Trampolines and dispatch table for the LMDB 1.0 engine. This TU sees only
 * the 1.0 stream's types and (renamed) symbols; the mdb_* names below expand
 * to mdb10_* via the rename header. */
#define ANYLMDB_STREAM 10
#include "anylmdb_rename10.h"
#include "lmdb.h"
#include "anylmdb_ops.h"

_Static_assert(sizeof(size_t) == 8, "anylmdb requires a 64-bit build");
_Static_assert(sizeof(mdb_size_t) == sizeof(size_t), "mdb_size_t must be size_t (no MDB_VL32)");
_Static_assert(sizeof(MDB_val) == 2 * sizeof(void *), "unexpected MDB_val layout");

#define ANYLMDB_T(ret, name, params, args) \
    static ret t_##name params { return mdb_##name args; }
ANYLMDB_OPS_COMMON(ANYLMDB_T)
ANYLMDB_OPS_V10(ANYLMDB_T)
#undef ANYLMDB_T

#define ANYLMDB_TV(ret, name, params, args) \
    static void t_##name params { mdb_##name args; }
ANYLMDB_OPS_COMMON_VOID(ANYLMDB_TV)
#undef ANYLMDB_TV

static void t_assert_cb(MDB_env *env, const char *msg)
{
    anylmdb_dispatch_assert(mdb_env_get_userctx(env), msg);
}

static int t_set_assert(void *env, int install)
{
    return mdb_env_set_assert((MDB_env *)env, install ? t_assert_cb : NULL);
}

const anylmdb_ops anylmdb_ops10 = {
#define ANYLMDB_I(ret, name, params, args) .name = t_##name,
    ANYLMDB_OPS_COMMON(ANYLMDB_I)
    ANYLMDB_OPS_COMMON_VOID(ANYLMDB_I)
    ANYLMDB_OPS_V10(ANYLMDB_I)
#undef ANYLMDB_I
    .set_assert = t_set_assert,
};
