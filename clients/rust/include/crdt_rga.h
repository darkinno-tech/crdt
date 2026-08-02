#ifndef DARKINNO_CRDT_RGA_H
#define DARKINNO_CRDT_RGA_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct crdt_rga crdt_rga;
typedef struct crdt_lww_map crdt_lww_map;

typedef struct {
  uint8_t *data;
  size_t len;
} crdt_buffer;

typedef struct {
  crdt_buffer replica_id;
  uint64_t wall_time;
  uint64_t logical;
} crdt_clock_state;

typedef struct {
  size_t max_frame_bytes;
  size_t max_payload_bytes;
  size_t max_string_bytes;
  size_t max_nodes;
  size_t max_tags;
  size_t max_tombstones;
  size_t max_pending_nodes;
  size_t max_pending_bytes;
} crdt_limits;

typedef struct {
  size_t max_frame_bytes;
  size_t max_payload_bytes;
  size_t max_string_bytes;
  size_t max_entries;
  size_t max_tombstones;
} crdt_lww_map_limits;

enum {
  CRDT_OK = 0,
  CRDT_INVALID_ARGUMENT = 1,
  CRDT_INVALID_FRAME = 2,
  CRDT_RESOURCE_LIMIT = 3,
  CRDT_PROTOCOL_MISMATCH = 4,
  CRDT_INVALID_DELTA = 5,
  CRDT_RANGE = 6,
  CRDT_INTERNAL = 7,
};

int32_t crdt_rga_new(const uint8_t *replica, size_t replica_len, crdt_rga **out);
int32_t crdt_rga_new_with_limits(const uint8_t *replica, size_t replica_len, const crdt_limits *limits, crdt_rga **out);
int32_t crdt_rga_new_from_clock(const uint8_t *replica, size_t replica_len, uint64_t wall_time, uint64_t logical, crdt_rga **out);
void crdt_rga_free(crdt_rga *value);
int32_t crdt_rga_apply(crdt_rga *value, const uint8_t *frame, size_t frame_len);
int32_t crdt_rga_insert(crdt_rga *value, size_t offset, const uint8_t *text, size_t text_len, crdt_buffer *out);
int32_t crdt_rga_delete(crdt_rga *value, size_t offset, size_t count, crdt_buffer *out);
int32_t crdt_rga_state(crdt_rga *value, crdt_buffer *out);
int32_t crdt_rga_clock_state(crdt_rga *value, crdt_clock_state *out);
int32_t crdt_rga_text(crdt_rga *value, crdt_buffer *out);
void crdt_buffer_free(crdt_buffer value);
void crdt_clock_state_free(crdt_clock_state value);

int32_t crdt_lww_map_new(const uint8_t *replica, size_t replica_len, crdt_lww_map **out);
int32_t crdt_lww_map_new_with_limits(const uint8_t *replica, size_t replica_len, const crdt_lww_map_limits *limits, crdt_lww_map **out);
int32_t crdt_lww_map_new_from_clock(const uint8_t *replica, size_t replica_len, uint64_t wall_time, uint64_t logical, crdt_lww_map **out);
int32_t crdt_lww_map_new_from_clock_with_limits(const uint8_t *replica, size_t replica_len, uint64_t wall_time, uint64_t logical, const crdt_lww_map_limits *limits, crdt_lww_map **out);
void crdt_lww_map_free(crdt_lww_map *value);
int32_t crdt_lww_map_apply(crdt_lww_map *value, const uint8_t *frame, size_t frame_len);
int32_t crdt_lww_map_set(crdt_lww_map *value, const uint8_t *key, size_t key_len, const uint8_t *bytes, size_t bytes_len, crdt_buffer *out);
int32_t crdt_lww_map_delete(crdt_lww_map *value, const uint8_t *key, size_t key_len, crdt_buffer *out);
int32_t crdt_lww_map_state(crdt_lww_map *value, crdt_buffer *out);
int32_t crdt_lww_map_clock_state(crdt_lww_map *value, crdt_clock_state *out);
int32_t crdt_lww_map_get(crdt_lww_map *value, const uint8_t *key, size_t key_len, crdt_buffer *out, uint8_t *present);
int32_t crdt_lww_map_keys(crdt_lww_map *value, crdt_buffer *out);

#ifdef __cplusplus
}
#endif

#endif
