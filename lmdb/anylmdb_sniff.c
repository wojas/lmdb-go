/* Format detection and the anylmdb_* extension API.
 *
 * The sniffer reads the data file's first meta page directly and never
 * opens an environment: letting the wrong engine attempt the open is not
 * safe (a failed LMDB 1.0 open can rewrite the lock file before it notices
 * the data-format mismatch).
 */
#include <errno.h>
#include <fcntl.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "anylmdb_int.h"

/* Meta page layout facts (see mdb.c of both streams):
 * both formats start the MDB_meta (mm_magic, mm_version, ...) right after
 * the page header, whose size is 16 bytes in 0.9 and 24 in 1.0 (1.0 added
 * an 8-byte mh_txnid). mm_magic is 0xBEEFC0DE in both; mm_version
 * (MDB_DATA_VERSION) is 1 in 0.9 and 3 in 1.0. Version 2 was written by
 * lmdb-js prerelease snapshots and is compatible with neither. */
#define ANYLMDB_MAGIC 0xBEEFC0DEu

static atomic_int anylmdb_default_ver;

int
anylmdb_set_default_version(anylmdb_ver ver)
{
    switch (ver) {
    case ANYLMDB_VER_DEFAULT:
    case ANYLMDB_VER_09:
    case ANYLMDB_VER_10:
        atomic_store(&anylmdb_default_ver, (int)ver);
        return MDB_SUCCESS;
    default:
        return EINVAL;
    }
}

int
anylmdb__default_version(void)
{
    int v = atomic_load(&anylmdb_default_ver);
    if (v == ANYLMDB_VER_09 || v == ANYLMDB_VER_10)
        return v;
    const char *e = getenv("ANYLMDB_DEFAULT");
    if (e) {
        if (strcmp(e, "10") == 0)
            return ANYLMDB_VER_10;
        if (strcmp(e, "09") == 0 || strcmp(e, "9") == 0)
            return ANYLMDB_VER_09;
    }
    return ANYLMDB_VER_09;
}

int
anylmdb_env_set_version(MDB_env *env, anylmdb_ver ver)
{
    if (!env || env->opened)
        return EINVAL;
    switch (ver) {
    case ANYLMDB_VER_DEFAULT:
    case ANYLMDB_VER_09:
    case ANYLMDB_VER_10:
        env->req_ver = (int)ver;
        return MDB_SUCCESS;
    default:
        return EINVAL;
    }
}

int
anylmdb_env_get_version(MDB_env *env, anylmdb_ver *ver)
{
    if (!env || !ver)
        return EINVAL;
    *ver = env->opened ? (anylmdb_ver)env->ver : (anylmdb_ver)env->req_ver;
    return MDB_SUCCESS;
}

static uint32_t
get32(const unsigned char *p)
{
    uint32_t v;
    memcpy(&v, p, sizeof v);
    return v;
}

int
anylmdb_sniff(const char *path, unsigned int flags, anylmdb_ver *ver)
{
    if (!path || !ver)
        return EINVAL;

    char *datafile;
    if (flags & MDB_NOSUBDIR) {
        datafile = strdup(path);
    } else {
        size_t n = strlen(path) + sizeof "/data.mdb";
        datafile = malloc(n);
        if (datafile)
            snprintf(datafile, n, "%s/data.mdb", path);
    }
    if (!datafile)
        return ENOMEM;

    int fd = open(datafile, O_RDONLY | O_CLOEXEC);
    free(datafile);
    if (fd < 0) {
        if (errno == ENOENT) {
            *ver = ANYLMDB_VER_NEW;
            return MDB_SUCCESS;
        }
        return errno;
    }

    unsigned char buf[32];
    ssize_t n = pread(fd, buf, sizeof buf, 0);
    int serrno = errno;
    close(fd);
    if (n == 0) {
        *ver = ANYLMDB_VER_NEW;
        return MDB_SUCCESS;
    }
    if (n < 0)
        return serrno;
    if ((size_t)n < sizeof buf)
        return MDB_INVALID;

    unsigned mmver;
    if (get32(buf + 16) == ANYLMDB_MAGIC)       /* 0.9 page header size */
        mmver = get32(buf + 20);
    else if (get32(buf + 24) == ANYLMDB_MAGIC)  /* 1.0 page header size */
        mmver = get32(buf + 28);
    else
        return MDB_INVALID;

    switch (mmver) {
    case 1:
        *ver = ANYLMDB_VER_09;
        return MDB_SUCCESS;
    case 3:
        *ver = ANYLMDB_VER_10;
        return MDB_SUCCESS;
    case 2:
        return ANYLMDB_FORMAT_UNSUPPORTED;
    default:
        /* valid magic, unknown data version (e.g. MDB_DEVEL builds) */
        return MDB_VERSION_MISMATCH;
    }
}
