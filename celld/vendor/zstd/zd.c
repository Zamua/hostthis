// The decompression half of libzstd's streaming API, exported for
// src/public/zstd.js. One context per decode. A zd_io is the
// ZSTD_inBuffer/ZSTD_outBuffer pair the caller fills before each zd_stream.
#define ZSTD_STATIC_LINKING_ONLY
#include "zstd.h"
#include <stdlib.h>

typedef struct { ZSTD_inBuffer in; ZSTD_outBuffer out; } zd_io;

// windowLogMax makes a frame declaring a larger window fail before the
// context allocates for it.
__attribute__((export_name("zd_create")))
ZSTD_DCtx* zd_create(int windowLogMax) {
  ZSTD_DCtx* d = ZSTD_createDCtx();
  if (d && ZSTD_isError(ZSTD_DCtx_setParameter(d, ZSTD_d_windowLogMax, windowLogMax))) {
    ZSTD_freeDCtx(d);
    return 0;
  }
  return d;
}

// Keeps the parameters, so a pooled context stays capped.
__attribute__((export_name("zd_reset")))
size_t zd_reset(ZSTD_DCtx* d) { return ZSTD_DCtx_reset(d, ZSTD_reset_session_only); }

__attribute__((export_name("zd_free")))
void zd_free(ZSTD_DCtx* d) { ZSTD_freeDCtx(d); }

__attribute__((export_name("zd_stream")))
size_t zd_stream(ZSTD_DCtx* d, zd_io* io) { return ZSTD_decompressStream(d, &io->out, &io->in); }

__attribute__((export_name("zd_is_error")))
unsigned zd_is_error(size_t code) { return ZSTD_isError(code); }

__attribute__((export_name("zd_error_name")))
const char* zd_error_name(size_t code) { return ZSTD_getErrorName(code); }

__attribute__((export_name("zd_in_size")))
size_t zd_in_size(void) { return ZSTD_DStreamInSize(); }

__attribute__((export_name("zd_out_size")))
size_t zd_out_size(void) { return ZSTD_DStreamOutSize(); }

__attribute__((export_name("zd_malloc")))
void* zd_malloc(size_t n) { return malloc(n); }

__attribute__((export_name("zd_mfree")))
void zd_mfree(void* p) { free(p); }
