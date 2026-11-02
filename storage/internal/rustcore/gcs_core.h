/* C ABI of gcs_core v2 (hand-written; keep in sync with gcs_core/src/*.rs).
 *
 * Ownership rules:
 *  - Strings in GcsStatus and strings returned by gcs_core are owned by the caller after the
 *    call and must be released with gcs_string_free.
 *  - Async functions return 0 (completed synchronously), 1 (pending: the GcsDoneCb fires exactly
 *    once) or -1 (failed synchronously: *out_status is set, no callback).
 *  - Memory lent to a pending operation must stay valid (pinned) until its callback fires.
 */
#ifndef GCS_CORE_V2_H
#define GCS_CORE_V2_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct GcsClient GcsClient;
typedef struct GcsReader GcsReader;
typedef struct GcsWriter GcsWriter;

enum { GCS_OK = 0, GCS_ERR = 1, GCS_PANIC = 2, GCS_INVALID = 3, GCS_CANCELLED = 4 };
enum { GCS_AUTH_ADC = 0, GCS_AUTH_ANONYMOUS = 1, GCS_AUTH_JSON = 2, GCS_AUTH_TOKEN_CALLBACK = 3 };
enum {
  GCS_COND_GENERATION_MATCH = 1,
  GCS_COND_GENERATION_NOT_MATCH = 2,
  GCS_COND_METAGENERATION_MATCH = 4,
  GCS_COND_METAGENERATION_NOT_MATCH = 8
};
enum { GCS_RETRY_DEFAULT = 0, GCS_RETRY_ALWAYS = 1, GCS_RETRY_NEVER = 2 };

typedef struct GcsStatus {
  int32_t code;
  int32_t http_code;
  int32_t rpc_code;
  char *message;
  char *headers;
} GcsStatus;

typedef void (*GcsDoneCb)(uintptr_t user, GcsStatus status, uint64_t n);
typedef int32_t (*GcsTokenCb)(uintptr_t user, char **out_token, int64_t *out_expiry_unix, char **out_err);

typedef struct GcsClientConfig {
  const char *endpoint;
  const char *universe_domain;
  const char *credentials_json;
  const char *quota_project;
  const char *user_agent;
  const char *custom_headers;
  int32_t auth_mode;
  GcsTokenCb token_cb;
  uintptr_t token_user;
} GcsClientConfig;

typedef struct GcsRetry {
  int32_t mode;               /* GCS_RETRY_* */
  int32_t max_attempts;       /* 0 = no limit */
  int64_t max_retry_duration_ms; /* 0 = no limit */
  int64_t backoff_initial_ms; /* 0 = SDK default */
  int64_t backoff_max_ms;
  double backoff_multiplier;
} GcsRetry;

typedef struct GcsReadParams {
  const char *bucket;
  const char *object;
  int64_t generation;
  int64_t offset;
  int64_t length;
  uint32_t cond_flags;
  int64_t if_generation_match;
  int64_t if_generation_not_match;
  int64_t if_metageneration_match;
  int64_t if_metageneration_not_match;
  const uint8_t *encryption_key;
  size_t encryption_key_len;
  int32_t read_compressed;
  int32_t disable_crc_check;
  size_t prefetch_bytes;
  GcsRetry retry;
} GcsReadParams;

typedef struct GcsWriteParams {
  const char *bucket;
  const char *object;
  const char *attrs_json;
  uint32_t cond_flags;
  int64_t if_generation_match;
  int64_t if_generation_not_match;
  int64_t if_metageneration_match;
  int64_t if_metageneration_not_match;
  const uint8_t *encryption_key;
  size_t encryption_key_len;
  size_t chunk_size;
  GcsRetry retry;
} GcsWriteParams;

void gcs_string_free(char *s);
uint64_t gcs_noop(uint64_t x);
int32_t gcs_async_noop(GcsDoneCb cb, uintptr_t user);
int32_t gcs_runtime_configure(uint32_t worker_threads);
void gcs_op_cancel(uint64_t op_id);

GcsStatus gcs_client_new(const GcsClientConfig *cfg, GcsClient **out);
void gcs_client_free(GcsClient *c);

int32_t gcs_reader_open(const GcsClient *c, const GcsReadParams *p, GcsDoneCb cb, uintptr_t user,
                        uint64_t *out_op, GcsStatus *out_status);
const char *gcs_reader_attrs_json(const GcsReader *r);
int32_t gcs_reader_read(const GcsReader *r, uint8_t *buf, size_t cap, GcsDoneCb cb, uintptr_t user,
                        size_t *out_n, GcsStatus *out_status);
void gcs_reader_cancel(const GcsReader *r);
void gcs_reader_close(GcsReader *r);

int32_t gcs_writer_open(const GcsClient *c, const GcsWriteParams *p, GcsWriter **out,
                        GcsStatus *out_status);
int32_t gcs_writer_write(const GcsWriter *w, const uint8_t *data, size_t len, GcsDoneCb cb,
                         uintptr_t user, GcsStatus *out_status);
uint64_t gcs_writer_accepted(const GcsWriter *w);
int32_t gcs_writer_close(const GcsWriter *w, GcsDoneCb cb, uintptr_t user, GcsStatus *out_status);
void gcs_writer_abort(const GcsWriter *w);
void gcs_writer_free(GcsWriter *w);

#endif /* GCS_CORE_V2_H */
