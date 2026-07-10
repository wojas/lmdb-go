/* anylmdb internal wrapper handle definitions, shared by anylmdb_core.c and
 * anylmdb_sniff.c. These structs complete the opaque public typedefs, so
 * this header must only be included by wrapper TUs (never by engine or glue
 * TUs, whose MDB_* types are the engine's own). */
#ifndef ANYLMDB_INT_H
#define ANYLMDB_INT_H

#include "anylmdb.h"
#include "anylmdb_ops.h"

struct MDB_env {
    const anylmdb_ops *ops;  /* set on successful open, together with real */
    void *real;              /* engine env; NULL before open / after failed open */
    int ver;                 /* 9 or 10 once opened */
    int opened;
    int req_ver;             /* anylmdb_env_set_version(); 0 = unset */
    void *userctx;           /* user's ctx; the ENGINE userctx always points
                              * back at this wrapper (assert trampoline) */
    MDB_assert_func *assert_func;
    /* settings buffered until mdb_env_open() picks an engine: */
    unsigned pending_flags;
    size_t mapsize;
    unsigned maxreaders;
    unsigned maxdbs;
    int pagesize;
    MDB_enc_func *enc_func;
    void *enc_key;           /* malloc'd copy of the key bytes */
    size_t enc_keylen;
    unsigned enc_size;
    MDB_sum_func *sum_func;
    unsigned sum_size;
    unsigned mapsize_set:1, maxreaders_set:1, maxdbs_set:1, assert_set:1,
             pagesize_set:1, enc_set:1, sum_set:1;
};

struct MDB_txn {
    MDB_env *env;
    MDB_txn *parent;
    void *real;
    unsigned rdonly:1;
    struct MDB_cursor *cursors;  /* shells of this txn's open cursors. Write
                                  * txn: freed when the engine auto-closes
                                  * them at commit/abort. Read-only txn:
                                  * detached at commit/abort (engine cursor
                                  * closed early, shell stays alive until the
                                  * app closes or renews it). */
};

struct MDB_cursor {
    /* ops/real must not be reached through txn: a read-only cursor may
     * legally be closed or renewed after its txn ended (documented 0.9
     * contract, restored on the 1.0 engine by detaching — see
     * anylmdb_txn_detach_cursors in anylmdb_core.c). */
    const anylmdb_ops *ops;
    MDB_txn *txn;                /* NULL once its read-only txn ended */
    void *real;                  /* NULL once detached */
    MDB_dbi dbi;                 /* for the lazy re-create in cursor_renew */
    struct MDB_cursor *next;
    unsigned on_list:1;
};

/* anylmdb_sniff.c */
int anylmdb__default_version(void);

#endif /* ANYLMDB_INT_H */
