/* C ABI of libiets_ffi, the iets Nix evaluator as oar links it. */
#ifndef OAR_IETS_FFI_H
#define OAR_IETS_FFI_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Evaluates `expr` strictly and renders the value as JSON.
 *
 *   system        the Nix system double, e.g. "x86_64-linux"
 *   project_root  absolute directory a pure evaluation may read below
 *                 (the nixpkgs checkout); NULL evaluates impurely
 *   cache_dir     where the compiled-file cache lives; NULL for iets's default
 *   workers       evaluator threads; 0 for iets's default
 *
 * Returns 0 and sets *json_out on success. Otherwise returns the exit code
 * the iets command line would have used and sets *error_out to the rendered
 * report. Either output may be NULL; a non-NULL one is freed with
 * oar_iets_free. The evaluator may still end the process on its own for an
 * out-of-memory condition or an internal fault: run it in a process you can
 * afford to lose.
 */
int32_t oar_iets_eval(const char *expr,
                      const char *system,
                      const char *project_root,
                      const char *cache_dir,
                      uint32_t workers,
                      char **json_out,
                      char **error_out);

void oar_iets_free(char *p);

/* "iets <version>+<build id prefix>"; static, never freed. */
const char *oar_iets_version(void);

#ifdef __cplusplus
}
#endif

#endif
