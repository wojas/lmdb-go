/* anylmdb core: the public mdb_* entrypoints.
 *
 * Every handle the application sees is a small wrapper: MDB_env carries the
 * engine dispatch table (chosen at mdb_env_open by sniffing the data file)
 * plus settings buffered before open; MDB_txn and MDB_cursor are thin
 * shells around the engine's own handles. Txn/cursor semantics are pure
 * passthrough with one exception: when a read-only txn ends, the engine
 * cursors are closed early so that the documented close/renew-after-txn-end
 * contract keeps working on the 1.0 engine (see anylmdb_txn_detach_cursors
 * and README).
 */
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "anylmdb_int.h"

/*
 * Globals
 */

static char anylmdb_version_str[192];

static void
anylmdb_version_init(void)
{
    snprintf(anylmdb_version_str, sizeof anylmdb_version_str,
        "ANYLMDB " ANYLMDB_VERSION_STRING ": %s + %s",
        anylmdb_ops10.version(NULL, NULL, NULL),
        anylmdb_ops09.version(NULL, NULL, NULL));
}

char *
mdb_version(int *major, int *minor, int *patch)
{
    /* Numeric identity is the shipped (1.0) header's: code that gates 1.0
     * workarounds on major >= 1 degrades safely on 0.9 environments,
     * whereas reporting 0.9 against a 1.0 engine could let it crash. */
    static pthread_once_t once = PTHREAD_ONCE_INIT;
    if (major) *major = MDB_VERSION_MAJOR;
    if (minor) *minor = MDB_VERSION_MINOR;
    if (patch) *patch = MDB_VERSION_PATCH;
    pthread_once(&once, anylmdb_version_init);
    return anylmdb_version_str;
}

char *
mdb_strerror(int err)
{
    if (err == ANYLMDB_FORMAT_UNSUPPORTED)
        return "anylmdb: unsupported LMDB data format (mm_version=2, lmdb-js prerelease?)";
    /* The 1.0 table is a superset of 0.9's and falls back to strerror(3). */
    return anylmdb_ops10.strerror(err);
}

void
anylmdb_dispatch_assert(void *wrapper_env, const char *msg)
{
    MDB_env *env = wrapper_env;
    if (env && env->assert_func)
        env->assert_func(env, msg);
}

/*
 * Environment
 */

int
mdb_env_create(MDB_env **envp)
{
    if (!envp)
        return EINVAL;
    MDB_env *env = calloc(1, sizeof *env);
    if (!env)
        return ENOMEM;
    *envp = env;
    return MDB_SUCCESS;
}

int
mdb_env_open(MDB_env *env, const char *path, unsigned int flags, mdb_mode_t mode)
{
    if (!env || !path)
        return EINVAL;
    if (env->opened)
        return EINVAL;

    anylmdb_ver sniffed;
    int rc = anylmdb_sniff(path, flags, &sniffed);
    if (rc)
        return rc;
    int use = (sniffed == ANYLMDB_VER_NEW)
        ? (env->req_ver ? env->req_ver : anylmdb__default_version())
        : (int)sniffed;

    const anylmdb_ops *ops = (use == ANYLMDB_VER_10) ? &anylmdb_ops10 : &anylmdb_ops09;
    if (!ops->env_set_pagesize && (env->pagesize_set || env->enc_set || env->sum_set))
        return ENOTSUP; /* 1.0-only settings buffered, 0.9 format chosen */

    void *real = NULL;
    rc = ops->env_create(&real);
    if (rc)
        return rc;
    ops->env_set_userctx(real, env); /* back-pointer for callbacks */

    if (env->pending_flags)
        rc = ops->env_set_flags(real, env->pending_flags, 1);
    if (!rc && env->mapsize_set)
        rc = ops->env_set_mapsize(real, env->mapsize);
    if (!rc && env->maxdbs_set)
        rc = ops->env_set_maxdbs(real, env->maxdbs);
    if (!rc && env->maxreaders_set)
        rc = ops->env_set_maxreaders(real, env->maxreaders);
    if (!rc && env->assert_set)
        rc = ops->set_assert(real, env->assert_func != NULL);
    if (!rc && env->pagesize_set)
        rc = ops->env_set_pagesize(real, env->pagesize);
    if (!rc && env->enc_set) {
        MDB_val key = { env->enc_keylen, env->enc_key };
        rc = ops->env_set_encrypt(real, (anylmdb_fnptr)env->enc_func, &key, env->enc_size);
    }
    if (!rc && env->sum_set)
        rc = ops->env_set_checksum(real, (anylmdb_fnptr)env->sum_func, env->sum_size);
    if (!rc)
        rc = ops->env_open(real, path, flags, mode);
    if (rc) {
        ops->env_close(real);
        return rc; /* like upstream, the env must still be closed by the app */
    }
    env->ops = ops;
    env->real = real;
    env->ver = use;
    env->opened = 1;
    return MDB_SUCCESS;
}

void
mdb_env_close(MDB_env *env)
{
    if (!env)
        return;
    if (env->real)
        env->ops->env_close(env->real);
    free(env->enc_key);
    free(env);
}

/* Most env calls only make sense on an opened env; upstream would crash or
 * misbehave on an unopened one. */
#define REQUIRE_OPEN(env) \
    do { if (!(env) || !(env)->opened) return EINVAL; } while (0)

int
mdb_env_copy(MDB_env *env, const char *path)
{
    REQUIRE_OPEN(env);
    return env->ops->env_copy(env->real, path);
}

int
mdb_env_copy2(MDB_env *env, const char *path, unsigned int flags)
{
    REQUIRE_OPEN(env);
    return env->ops->env_copy2(env->real, path, flags);
}

int
mdb_env_copyfd(MDB_env *env, mdb_filehandle_t fd)
{
    REQUIRE_OPEN(env);
    return env->ops->env_copyfd(env->real, fd);
}

int
mdb_env_copyfd2(MDB_env *env, mdb_filehandle_t fd, unsigned int flags)
{
    REQUIRE_OPEN(env);
    return env->ops->env_copyfd2(env->real, fd, flags);
}

int
mdb_env_stat(MDB_env *env, MDB_stat *stat)
{
    REQUIRE_OPEN(env);
    return env->ops->env_stat(env->real, stat);
}

int
mdb_env_info(MDB_env *env, MDB_envinfo *info)
{
    REQUIRE_OPEN(env);
    return env->ops->env_info(env->real, info);
}

int
mdb_env_sync(MDB_env *env, int force)
{
    REQUIRE_OPEN(env);
    return env->ops->env_sync(env->real, force);
}

/* Runtime-changeable env flags, identical in 0.9.35 and 1.0.0 (CHANGEABLE
 * in mdb.c). Upstream mdb_env_set_flags rejects anything else immediately,
 * so buffered pre-open calls must do the same rather than defer the EINVAL
 * to mdb_env_open. This also keeps pending_flags within the set upstream's
 * mdb_env_get_flags would expose. */
#define ANYLMDB_FLAGS_CHANGEABLE \
    (MDB_NOSYNC|MDB_NOMETASYNC|MDB_MAPASYNC|MDB_NOMEMINIT)

int
mdb_env_set_flags(MDB_env *env, unsigned int flags, int onoff)
{
    if (!env)
        return EINVAL;
    if (env->opened)
        return env->ops->env_set_flags(env->real, flags, onoff);
    if (flags & ~ANYLMDB_FLAGS_CHANGEABLE)
        return EINVAL;
    if (onoff)
        env->pending_flags |= flags;
    else
        env->pending_flags &= ~flags;
    return MDB_SUCCESS;
}

int
mdb_env_get_flags(MDB_env *env, unsigned int *flags)
{
    if (!env || !flags)
        return EINVAL;
    if (env->opened)
        return env->ops->env_get_flags(env->real, flags);
    *flags = env->pending_flags;
    return MDB_SUCCESS;
}

int
mdb_env_get_path(MDB_env *env, const char **path)
{
    if (!env || !path)
        return EINVAL;
    if (!env->opened) {
        *path = NULL; /* matches upstream: me_path is NULL before open */
        return MDB_SUCCESS;
    }
    return env->ops->env_get_path(env->real, path);
}

int
mdb_env_get_fd(MDB_env *env, mdb_filehandle_t *fd)
{
    if (!env || !fd)
        return EINVAL;
    if (!env->opened) {
        *fd = (mdb_filehandle_t)-1; /* matches upstream: me_fd invalid before open */
        return MDB_SUCCESS;
    }
    return env->ops->env_get_fd(env->real, fd);
}

int
mdb_env_set_mapsize(MDB_env *env, mdb_size_t size)
{
    if (!env)
        return EINVAL;
    if (env->opened)
        return env->ops->env_set_mapsize(env->real, size);
    env->mapsize = size;
    env->mapsize_set = 1;
    return MDB_SUCCESS;
}

int
mdb_env_set_maxreaders(MDB_env *env, unsigned int readers)
{
    if (!env)
        return EINVAL;
    if (env->opened) /* engine returns EINVAL, as upstream documents */
        return env->ops->env_set_maxreaders(env->real, readers);
    env->maxreaders = readers;
    env->maxreaders_set = 1;
    return MDB_SUCCESS;
}

int
mdb_env_get_maxreaders(MDB_env *env, unsigned int *readers)
{
    if (!env || !readers)
        return EINVAL;
    if (env->opened)
        return env->ops->env_get_maxreaders(env->real, readers);
    *readers = env->maxreaders_set ? env->maxreaders : 126; /* upstream default */
    return MDB_SUCCESS;
}

int
mdb_env_set_maxdbs(MDB_env *env, MDB_dbi dbs)
{
    if (!env)
        return EINVAL;
    if (env->opened)
        return env->ops->env_set_maxdbs(env->real, dbs);
    env->maxdbs = dbs;
    env->maxdbs_set = 1;
    return MDB_SUCCESS;
}

int
mdb_env_get_maxkeysize(MDB_env *env)
{
    /* Constant 511 in 0.9; 1.0 computes it from the env (and crashes on
     * NULL). Answer for NULL/unopened envs ourselves. */
    if (!env || !env->opened)
        return 511;
    return env->ops->env_get_maxkeysize(env->real);
}

int
mdb_env_set_userctx(MDB_env *env, void *ctx)
{
    if (!env)
        return EINVAL;
    /* Never forwarded: the engine's userctx is our back-pointer. */
    env->userctx = ctx;
    return MDB_SUCCESS;
}

void *
mdb_env_get_userctx(MDB_env *env)
{
    return env ? env->userctx : NULL;
}

int
mdb_env_set_assert(MDB_env *env, MDB_assert_func *func)
{
    if (!env)
        return EINVAL;
    env->assert_func = func;
    env->assert_set = 1;
    if (env->opened)
        return env->ops->set_assert(env->real, func != NULL);
    return MDB_SUCCESS;
}

int
mdb_reader_list(MDB_env *env, MDB_msg_func *func, void *ctx)
{
    if (!env || !func)
        return -1; /* upstream returns -1 here, not an errno */
    if (!env->opened) /* upstream with no lock table yet */
        return func("(no reader locks)\n", ctx);
    return env->ops->reader_list(env->real, (anylmdb_fnptr)func, ctx);
}

int
mdb_reader_check(MDB_env *env, int *dead)
{
    if (!env)
        return EINVAL;
    if (!env->opened) { /* upstream with no lock table yet */
        if (dead)
            *dead = 0;
        return MDB_SUCCESS;
    }
    return env->ops->reader_check(env->real, dead);
}

/*
 * Environment, LMDB 1.0 only. On a 0.9 environment these return ENOTSUP
 * (the ops slot is NULL). The pre-open setters are buffered because the
 * engine is not chosen yet; if mdb_env_open then picks/sniffs 0.9, the
 * open fails with ENOTSUP before touching anything.
 */

int
mdb_env_set_pagesize(MDB_env *env, int size)
{
    if (!env)
        return EINVAL;
    if (env->opened)
        return env->ops->env_set_pagesize
            ? env->ops->env_set_pagesize(env->real, size) : ENOTSUP;
    env->pagesize = size;
    env->pagesize_set = 1;
    return MDB_SUCCESS;
}

int
mdb_env_set_encrypt(MDB_env *env, MDB_enc_func *func, const MDB_val *key, unsigned int size)
{
    if (!env || !func || !key)
        return EINVAL;
    if (env->opened) {
        if (!env->ops->env_set_encrypt)
            return ENOTSUP;
        return env->ops->env_set_encrypt(env->real, (anylmdb_fnptr)func, key, size);
    }
    void *copy = malloc(key->mv_size ? key->mv_size : 1);
    if (!copy)
        return ENOMEM;
    memcpy(copy, key->mv_data, key->mv_size);
    free(env->enc_key);
    env->enc_key = copy;
    env->enc_keylen = key->mv_size;
    env->enc_func = func;
    env->enc_size = size;
    env->enc_set = 1;
    return MDB_SUCCESS;
}

int
mdb_env_set_checksum(MDB_env *env, MDB_sum_func *func, unsigned int size)
{
    if (!env || !func)
        return EINVAL;
    if (env->opened) {
        if (!env->ops->env_set_checksum)
            return ENOTSUP;
        return env->ops->env_set_checksum(env->real, (anylmdb_fnptr)func, size);
    }
    env->sum_func = func;
    env->sum_size = size;
    env->sum_set = 1;
    return MDB_SUCCESS;
}

#define V10_ENV_OP(env, op, ...) \
    do { \
        REQUIRE_OPEN(env); \
        if (!(env)->ops->op) \
            return ENOTSUP; \
        return (env)->ops->op((env)->real, __VA_ARGS__); \
    } while (0)

int
mdb_env_rollback(MDB_env *env, mdb_size_t txnid)
{
    V10_ENV_OP(env, env_rollback, txnid);
}

int
mdb_env_incr_dump(MDB_env *env, const char *path, size_t txnid)
{
    V10_ENV_OP(env, env_incr_dump, path, txnid);
}

int
mdb_env_incr_dumpfd(MDB_env *env, mdb_filehandle_t fd, size_t txnid)
{
    V10_ENV_OP(env, env_incr_dumpfd, fd, txnid);
}

int
mdb_env_incr_loadfd(MDB_env *env, mdb_filehandle_t fd)
{
    V10_ENV_OP(env, env_incr_loadfd, fd);
}

/*
 * Transactions
 */

static void
anylmdb_txn_detach_cursors(MDB_txn *txn)
{
    /* This read-only txn is about to end. Both LMDB versions document that
     * its cursors may still be closed (or renewed) afterwards, but LMDB
     * 1.0.0's mdb_cursor_close reads the freed txn (use-after-free under
     * the default MDB_RPAGE_CACHE, and the documented MDB_RPAGE_CACHE=0
     * opt-out does not compile). Keep the documented contract working on
     * both engines: close the engine cursors now, while the txn is still
     * live, and leave the app-facing shells alive. mdb_cursor_close on a
     * detached shell just frees it; mdb_cursor_renew re-creates an engine
     * cursor on the new txn (identical semantics — renew rebinds the
     * cursor and resets its position either way). */
    MDB_cursor *c = txn->cursors, *next;
    for (; c; c = next) {
        next = c->next;
        c->ops->cursor_close(c->real);
        c->real = NULL;
        c->txn = NULL;
        c->next = NULL;
        c->on_list = 0;
    }
    txn->cursors = NULL;
}

static void
anylmdb_txn_free(MDB_txn *txn)
{
    /* Write txn: the engine already closed the real cursors at commit/
     * abort; only our shells remain, and upstream forbids touching a
     * write-txn cursor afterwards. (For read-only txns the list was
     * emptied by anylmdb_txn_detach_cursors.) */
    MDB_cursor *c = txn->cursors, *next;
    for (; c; c = next) {
        next = c->next;
        free(c);
    }
    free(txn);
}

int
mdb_txn_begin(MDB_env *env, MDB_txn *parent, unsigned int flags, MDB_txn **ret)
{
    REQUIRE_OPEN(env);
    if (!ret)
        return EINVAL;
    MDB_txn *txn = calloc(1, sizeof *txn);
    if (!txn)
        return ENOMEM;
    int rc = env->ops->txn_begin(env->real, parent ? parent->real : NULL,
        flags, &txn->real);
    if (rc) {
        free(txn);
        return rc;
    }
    txn->env = env;
    txn->parent = parent;
    txn->rdonly = (flags & MDB_RDONLY) ? 1 : 0;
    *ret = txn;
    return MDB_SUCCESS;
}

MDB_env *
mdb_txn_env(MDB_txn *txn)
{
    return txn ? txn->env : NULL;
}

mdb_size_t
mdb_txn_id(MDB_txn *txn)
{
    if (!txn) /* upstream returns 0, not a crash */
        return 0;
    return txn->env->ops->txn_id(txn->real);
}

int
mdb_txn_commit(MDB_txn *txn)
{
    if (!txn)
        return EINVAL;
    if (txn->rdonly)
        anylmdb_txn_detach_cursors(txn);
    int rc = txn->env->ops->txn_commit(txn->real);
    anylmdb_txn_free(txn); /* upstream frees the txn even when commit fails */
    return rc;
}

void
mdb_txn_abort(MDB_txn *txn)
{
    if (!txn)
        return;
    if (txn->rdonly)
        anylmdb_txn_detach_cursors(txn);
    txn->env->ops->txn_abort(txn->real);
    anylmdb_txn_free(txn);
}

void
mdb_txn_reset(MDB_txn *txn)
{
    if (!txn) /* upstream tolerates NULL */
        return;
    txn->env->ops->txn_reset(txn->real);
}

int
mdb_txn_renew(MDB_txn *txn)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->txn_renew(txn->real);
}

int
mdb_txn_flags(MDB_txn *txn, unsigned int *flags)
{
    if (!txn || !flags)
        return EINVAL;
    if (!txn->env->ops->txn_flags)
        return ENOTSUP;
    return txn->env->ops->txn_flags(txn->real, flags);
}

int
mdb_txn_prepare(MDB_txn *txn)
{
    if (!txn)
        return EINVAL;
    if (!txn->env->ops->txn_prepare)
        return ENOTSUP;
    return txn->env->ops->txn_prepare(txn->real);
}

/*
 * Databases and data
 */

int
mdb_dbi_open(MDB_txn *txn, const char *name, unsigned int flags, MDB_dbi *dbi)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->dbi_open(txn->real, name, flags, dbi);
}

int
mdb_stat(MDB_txn *txn, MDB_dbi dbi, MDB_stat *stat)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->stat(txn->real, dbi, stat);
}

int
mdb_dbi_flags(MDB_txn *txn, MDB_dbi dbi, unsigned int *flags)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->dbi_flags(txn->real, dbi, flags);
}

void
mdb_dbi_close(MDB_env *env, MDB_dbi dbi)
{
    if (!env || !env->opened)
        return;
    env->ops->dbi_close(env->real, dbi);
}

int
mdb_drop(MDB_txn *txn, MDB_dbi dbi, int del)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->drop(txn->real, dbi, del);
}

int
mdb_set_compare(MDB_txn *txn, MDB_dbi dbi, MDB_cmp_func *cmp)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->set_compare(txn->real, dbi, (anylmdb_fnptr)cmp);
}

int
mdb_set_dupsort(MDB_txn *txn, MDB_dbi dbi, MDB_cmp_func *cmp)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->set_dupsort(txn->real, dbi, (anylmdb_fnptr)cmp);
}

int
mdb_set_relfunc(MDB_txn *txn, MDB_dbi dbi, MDB_rel_func *rel)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->set_relfunc(txn->real, dbi, (anylmdb_fnptr)rel);
}

int
mdb_set_relctx(MDB_txn *txn, MDB_dbi dbi, void *ctx)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->set_relctx(txn->real, dbi, ctx);
}

int
mdb_get(MDB_txn *txn, MDB_dbi dbi, MDB_val *key, MDB_val *data)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->get(txn->real, dbi, key, data);
}

int
mdb_put(MDB_txn *txn, MDB_dbi dbi, MDB_val *key, MDB_val *data, unsigned int flags)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->put(txn->real, dbi, key, data, flags);
}

int
mdb_del(MDB_txn *txn, MDB_dbi dbi, MDB_val *key, MDB_val *data)
{
    if (!txn)
        return EINVAL;
    return txn->env->ops->del(txn->real, dbi, key, data);
}

int
mdb_cmp(MDB_txn *txn, MDB_dbi dbi, const MDB_val *a, const MDB_val *b)
{
    return txn->env->ops->cmp(txn->real, dbi, a, b);
}

int
mdb_dcmp(MDB_txn *txn, MDB_dbi dbi, const MDB_val *a, const MDB_val *b)
{
    return txn->env->ops->dcmp(txn->real, dbi, a, b);
}

/*
 * Cursors
 */

int
mdb_cursor_open(MDB_txn *txn, MDB_dbi dbi, MDB_cursor **ret)
{
    if (!txn || !ret)
        return EINVAL;
    MDB_cursor *cursor = calloc(1, sizeof *cursor);
    if (!cursor)
        return ENOMEM;
    int rc = txn->env->ops->cursor_open(txn->real, dbi, &cursor->real);
    if (rc) {
        free(cursor);
        return rc;
    }
    cursor->ops = txn->env->ops;
    cursor->txn = txn;
    cursor->dbi = dbi;
    /* Track every shell on its txn: write-txn shells are freed when the
     * engine auto-closes the cursors at txn end, read-only shells are
     * detached then (anylmdb_txn_detach_cursors). Unlinking at explicit
     * close is always safe: on_list implies the txn is still live. */
    cursor->next = txn->cursors;
    txn->cursors = cursor;
    cursor->on_list = 1;
    *ret = cursor;
    return MDB_SUCCESS;
}

static void
anylmdb_cursor_unlink(MDB_cursor *cursor)
{
    MDB_cursor **prev = &cursor->txn->cursors;
    while (*prev && *prev != cursor)
        prev = &(*prev)->next;
    if (*prev == cursor)
        *prev = cursor->next;
    cursor->on_list = 0;
}

void
mdb_cursor_close(MDB_cursor *cursor)
{
    if (!cursor)
        return;
    if (cursor->real) /* NULL once its read-only txn ended */
        cursor->ops->cursor_close(cursor->real);
    if (cursor->on_list)
        anylmdb_cursor_unlink(cursor);
    free(cursor);
}

int
mdb_cursor_renew(MDB_txn *txn, MDB_cursor *cursor)
{
    if (!txn || !cursor)
        return EINVAL;
    int rc;
    if (cursor->real) {
        rc = txn->env->ops->cursor_renew(txn->real, cursor->real);
    } else {
        /* The engine cursor was closed when its read-only txn ended;
         * re-create it on the new txn. Same semantics: renew rebinds the
         * cursor and resets its position either way. */
        rc = txn->env->ops->cursor_open(txn->real, cursor->dbi, &cursor->real);
    }
    if (rc != MDB_SUCCESS)
        return rc;
    if (cursor->txn != txn) { /* move the shell to the new txn's list */
        if (cursor->on_list)
            anylmdb_cursor_unlink(cursor);
        cursor->txn = txn;
        cursor->next = txn->cursors;
        txn->cursors = cursor;
        cursor->on_list = 1;
    }
    return MDB_SUCCESS;
}

MDB_txn *
mdb_cursor_txn(MDB_cursor *cursor)
{
    return cursor ? cursor->txn : NULL;
}

MDB_dbi
mdb_cursor_dbi(MDB_cursor *cursor)
{
    return cursor->ops->cursor_dbi(cursor->real);
}

int
mdb_cursor_get(MDB_cursor *cursor, MDB_val *key, MDB_val *data, MDB_cursor_op op)
{
    if (!cursor)
        return EINVAL;
    return cursor->ops->cursor_get(cursor->real, key, data, (int)op);
}

int
mdb_cursor_put(MDB_cursor *cursor, MDB_val *key, MDB_val *data, unsigned int flags)
{
    if (!cursor)
        return EINVAL;
    return cursor->ops->cursor_put(cursor->real, key, data, flags);
}

int
mdb_cursor_del(MDB_cursor *cursor, unsigned int flags)
{
    if (!cursor)
        return EINVAL;
    return cursor->ops->cursor_del(cursor->real, flags);
}

int
mdb_cursor_count(MDB_cursor *cursor, mdb_size_t *countp)
{
    if (!cursor)
        return EINVAL;
    return cursor->ops->cursor_count(cursor->real, countp);
}

int
mdb_cursor_is_db(MDB_cursor *cursor)
{
    if (!cursor)
        return EINVAL;
    if (!cursor->ops->cursor_is_db)
        return ENOTSUP;
    return cursor->ops->cursor_is_db(cursor->real);
}

/*
 * Crypto module helpers. 1.0's module.c is not built (no dlopen dependency,
 * and it is under the Symas dual-use license rather than the OpenLDAP
 * Public License), so mdb_modload always fails. mdb_modsetup is
 * reimplemented here from its documented contract in lmdb.h — "just a
 * wrapper around mdb_env_set_encrypt()" — so statically provided crypto
 * hooks still work.
 */

void *
mdb_modload(const char *file, const char *symname,
    MDB_crypto_funcs **mcf_ptr, char **errmsg)
{
    (void)file; (void)symname; (void)mcf_ptr;
    if (errmsg)
        *errmsg = "anylmdb: dynamically loaded crypto modules are not supported";
    return NULL;
}

void
mdb_modunload(void *handle)
{
    (void)handle;
}

void
mdb_modsetup(MDB_env *env, MDB_crypto_funcs *cf, const char *password)
{
    if (!env || !cf)
        return;
    if (cf->mcf_sumfunc)
        mdb_env_set_checksum(env, cf->mcf_sumfunc, cf->mcf_sumsize);
    if (cf->mcf_encfunc && password) {
        char keydata[2048];
        MDB_val key = { (size_t)cf->mcf_keysize, keydata };
        if (cf->mcf_keysize <= 0 || key.mv_size > sizeof keydata)
            return;
        if (cf->mcf_str2key) {
            cf->mcf_str2key(password, &key);
        } else {
            memset(keydata, 0, key.mv_size);
            strncpy(keydata, password, key.mv_size);
        }
        mdb_env_set_encrypt(env, cf->mcf_encfunc, &key, cf->mcf_esumsize);
        memset(keydata, 0, sizeof keydata);
    }
}
