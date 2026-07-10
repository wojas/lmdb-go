/* Engine-neutral dispatch table between the anylmdb core and the two
 * renamed LMDB engines.
 *
 * This header must not include lmdb.h: the core TU sees the public (1.0)
 * types and each glue TU sees its own stream's types, so everything crosses
 * this boundary as void* (all engine handles are opaque pointers and
 * MDB_val/MDB_stat/MDB_envinfo are layout-identical across streams on
 * 64-bit non-VL32 builds) or as a generic function pointer.
 *
 * The X-macro lists below generate three things that can therefore never
 * drift apart: the anylmdb_ops struct fields, the per-stream trampoline
 * definitions, and the per-stream table initializers (see anylmdb_glue09.c
 * and anylmdb_glue10.c).
 *
 * X(ret, name, params, args):
 *   ret/params — the engine-neutral trampoline signature
 *   args       — the argument list with casts to the engine's own types,
 *                used only inside the glue TUs
 */
#ifndef ANYLMDB_OPS_H
#define ANYLMDB_OPS_H

#include <stddef.h>
#include <sys/types.h>

/* Generic carrier for user callbacks (MDB_cmp_func*, MDB_rel_func*,
 * MDB_msg_func*, ...). Glue trampolines cast back to the engine's exact
 * function-pointer type; the value is always the user's original pointer. */
typedef void (*anylmdb_fnptr)(void);

/* Implemented in anylmdb_core.c; called by the glue assert trampolines with
 * the wrapper MDB_env recovered from the engine userctx back-pointer. */
extern void anylmdb_dispatch_assert(void *wrapper_env, const char *msg);

/* Common surface: present in both LMDB 0.9 and 1.0. */
#define ANYLMDB_OPS_COMMON(X) \
X(int, env_create, (void **env), ((MDB_env **)env)) \
X(int, env_open, (void *env, const char *path, unsigned flags, mode_t mode), \
    ((MDB_env *)env, path, flags, mode)) \
X(int, env_copy, (void *env, const char *path), ((MDB_env *)env, path)) \
X(int, env_copy2, (void *env, const char *path, unsigned flags), \
    ((MDB_env *)env, path, flags)) \
X(int, env_copyfd, (void *env, int fd), ((MDB_env *)env, fd)) \
X(int, env_copyfd2, (void *env, int fd, unsigned flags), ((MDB_env *)env, fd, flags)) \
X(int, env_stat, (void *env, void *stat), ((MDB_env *)env, (MDB_stat *)stat)) \
X(int, env_info, (void *env, void *info), ((MDB_env *)env, (MDB_envinfo *)info)) \
X(int, env_sync, (void *env, int force), ((MDB_env *)env, force)) \
X(int, env_set_flags, (void *env, unsigned flags, int onoff), \
    ((MDB_env *)env, flags, onoff)) \
X(int, env_get_flags, (void *env, unsigned *flags), ((MDB_env *)env, flags)) \
X(int, env_get_path, (void *env, const char **path), ((MDB_env *)env, path)) \
X(int, env_get_fd, (void *env, int *fd), ((MDB_env *)env, fd)) \
X(int, env_set_mapsize, (void *env, size_t size), ((MDB_env *)env, size)) \
X(int, env_set_maxreaders, (void *env, unsigned readers), ((MDB_env *)env, readers)) \
X(int, env_get_maxreaders, (void *env, unsigned *readers), ((MDB_env *)env, readers)) \
X(int, env_set_maxdbs, (void *env, unsigned dbs), ((MDB_env *)env, dbs)) \
X(int, env_get_maxkeysize, (void *env), ((MDB_env *)env)) \
X(int, env_set_userctx, (void *env, void *ctx), ((MDB_env *)env, ctx)) \
X(void *, env_get_userctx, (void *env), ((MDB_env *)env)) \
X(int, txn_begin, (void *env, void *parent, unsigned flags, void **txn), \
    ((MDB_env *)env, (MDB_txn *)parent, flags, (MDB_txn **)txn)) \
X(size_t, txn_id, (void *txn), ((MDB_txn *)txn)) \
X(int, txn_commit, (void *txn), ((MDB_txn *)txn)) \
X(int, txn_renew, (void *txn), ((MDB_txn *)txn)) \
X(int, dbi_open, (void *txn, const char *name, unsigned flags, unsigned *dbi), \
    ((MDB_txn *)txn, name, flags, (MDB_dbi *)dbi)) \
X(int, stat, (void *txn, unsigned dbi, void *stat), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_stat *)stat)) \
X(int, dbi_flags, (void *txn, unsigned dbi, unsigned *flags), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, flags)) \
X(int, drop, (void *txn, unsigned dbi, int del), ((MDB_txn *)txn, (MDB_dbi)dbi, del)) \
X(int, set_compare, (void *txn, unsigned dbi, anylmdb_fnptr cmp), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_cmp_func *)cmp)) \
X(int, set_dupsort, (void *txn, unsigned dbi, anylmdb_fnptr cmp), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_cmp_func *)cmp)) \
X(int, set_relfunc, (void *txn, unsigned dbi, anylmdb_fnptr rel), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_rel_func *)rel)) \
X(int, set_relctx, (void *txn, unsigned dbi, void *ctx), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, ctx)) \
X(int, get, (void *txn, unsigned dbi, void *key, void *data), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_val *)key, (MDB_val *)data)) \
X(int, put, (void *txn, unsigned dbi, void *key, void *data, unsigned flags), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_val *)key, (MDB_val *)data, flags)) \
X(int, del, (void *txn, unsigned dbi, void *key, void *data), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_val *)key, (MDB_val *)data)) \
X(int, cursor_open, (void *txn, unsigned dbi, void **cursor), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (MDB_cursor **)cursor)) \
X(int, cursor_renew, (void *txn, void *cursor), \
    ((MDB_txn *)txn, (MDB_cursor *)cursor)) \
X(unsigned, cursor_dbi, (void *cursor), ((MDB_cursor *)cursor)) \
X(int, cursor_get, (void *cursor, void *key, void *data, int op), \
    ((MDB_cursor *)cursor, (MDB_val *)key, (MDB_val *)data, (MDB_cursor_op)op)) \
X(int, cursor_put, (void *cursor, void *key, void *data, unsigned flags), \
    ((MDB_cursor *)cursor, (MDB_val *)key, (MDB_val *)data, flags)) \
X(int, cursor_del, (void *cursor, unsigned flags), ((MDB_cursor *)cursor, flags)) \
X(int, cursor_count, (void *cursor, size_t *countp), \
    ((MDB_cursor *)cursor, countp)) \
X(int, cmp, (void *txn, unsigned dbi, const void *a, const void *b), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (const MDB_val *)a, (const MDB_val *)b)) \
X(int, dcmp, (void *txn, unsigned dbi, const void *a, const void *b), \
    ((MDB_txn *)txn, (MDB_dbi)dbi, (const MDB_val *)a, (const MDB_val *)b)) \
X(int, reader_list, (void *env, anylmdb_fnptr func, void *ctx), \
    ((MDB_env *)env, (MDB_msg_func *)func, ctx)) \
X(int, reader_check, (void *env, int *dead), ((MDB_env *)env, dead)) \
X(char *, strerror, (int err), (err)) \
X(char *, version, (int *major, int *minor, int *patch), (major, minor, patch))

/* Common surface, void-returning. */
#define ANYLMDB_OPS_COMMON_VOID(X) \
X(void, env_close, (void *env), ((MDB_env *)env)) \
X(void, txn_abort, (void *txn), ((MDB_txn *)txn)) \
X(void, txn_reset, (void *txn), ((MDB_txn *)txn)) \
X(void, dbi_close, (void *env, unsigned dbi), ((MDB_env *)env, (MDB_dbi)dbi)) \
X(void, cursor_close, (void *cursor), ((MDB_cursor *)cursor))

/* LMDB 1.0 only. These slots are NULL in anylmdb_ops09; the NULL check in
 * the core is what produces ENOTSUP on 0.9 environments. */
#define ANYLMDB_OPS_V10(X) \
X(int, env_set_pagesize, (void *env, int size), ((MDB_env *)env, size)) \
X(int, env_set_encrypt, (void *env, anylmdb_fnptr func, const void *key, unsigned size), \
    ((MDB_env *)env, (MDB_enc_func *)func, (const MDB_val *)key, size)) \
X(int, env_set_checksum, (void *env, anylmdb_fnptr func, unsigned size), \
    ((MDB_env *)env, (MDB_sum_func *)func, size)) \
X(int, txn_flags, (void *txn, unsigned *flags), ((MDB_txn *)txn, flags)) \
X(int, txn_prepare, (void *txn), ((MDB_txn *)txn)) \
X(int, env_rollback, (void *env, size_t txnid), ((MDB_env *)env, txnid)) \
X(int, env_incr_dump, (void *env, const char *path, size_t txnid), \
    ((MDB_env *)env, path, txnid)) \
X(int, env_incr_dumpfd, (void *env, int fd, size_t txnid), ((MDB_env *)env, fd, txnid)) \
X(int, env_incr_loadfd, (void *env, int fd), ((MDB_env *)env, fd)) \
X(int, cursor_is_db, (void *cursor), ((MDB_cursor *)cursor))

typedef struct anylmdb_ops {
#define ANYLMDB_F(ret, name, params, args) ret (*name) params;
    ANYLMDB_OPS_COMMON(ANYLMDB_F)
    ANYLMDB_OPS_COMMON_VOID(ANYLMDB_F)
    ANYLMDB_OPS_V10(ANYLMDB_F)
#undef ANYLMDB_F
    /* Irregular: installs (install=1) or clears (install=0) the stream's
     * assert trampoline, which routes through anylmdb_dispatch_assert. */
    int (*set_assert)(void *env, int install);
} anylmdb_ops;

extern const anylmdb_ops anylmdb_ops09;
extern const anylmdb_ops anylmdb_ops10;

#endif /* ANYLMDB_OPS_H */
