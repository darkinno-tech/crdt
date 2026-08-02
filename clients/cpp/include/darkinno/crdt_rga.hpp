#ifndef DARKINNO_CPP_CRDT_RGA_HPP
#define DARKINNO_CPP_CRDT_RGA_HPP

#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

extern "C" {
#include <crdt_rga.h>
}

namespace darkinno::crdt {

class Error final : public std::runtime_error {
 public:
  explicit Error(std::int32_t code) : std::runtime_error(Message(code)), code_(code) {}

  [[nodiscard]] std::int32_t Code() const noexcept { return code_; }

 private:
  static const char* Message(std::int32_t code) noexcept {
    switch (code) {
      case CRDT_INVALID_ARGUMENT:
        return "invalid CRDT RGA argument";
      case CRDT_INVALID_FRAME:
        return "invalid CRDT RGA frame";
      case CRDT_RESOURCE_LIMIT:
        return "CRDT RGA resource limit exceeded";
      case CRDT_PROTOCOL_MISMATCH:
        return "CRDT RGA protocol mismatch";
      case CRDT_INVALID_DELTA:
        return "invalid CRDT RGA delta";
      case CRDT_RANGE:
        return "invalid CRDT RGA visible range";
      default:
        return "internal CRDT RGA error";
    }
  }

  std::int32_t code_;
};

struct ClockState {
  std::vector<std::uint8_t> replica_id;
  std::uint64_t wall_time{};
  std::uint64_t logical{};
};

// Rga owns one opaque C ABI handle. It is movable but not copyable; a caller
// must ensure no method races handle destruction from another thread.
class Rga final {
 public:
  explicit Rga(std::string_view replica_id)
      : Rga(std::span<const std::uint8_t>(
            reinterpret_cast<const std::uint8_t*>(replica_id.data()), replica_id.size())) {}

  explicit Rga(std::span<const std::uint8_t> replica_id) : handle_(NewHandle(replica_id)) {}

  Rga(std::span<const std::uint8_t> replica_id, const crdt_limits& limits)
      : handle_(NewHandle(replica_id, &limits)) {}

  explicit Rga(const ClockState& clock_state) : handle_(NewHandle(clock_state)) {}

  ~Rga() { crdt_rga_free(handle_); }

  Rga(const Rga&) = delete;
  Rga& operator=(const Rga&) = delete;

  Rga(Rga&& other) noexcept : handle_(std::exchange(other.handle_, nullptr)) {}

  Rga& operator=(Rga&& other) noexcept {
    if (this != &other) {
      crdt_rga_free(handle_);
      handle_ = std::exchange(other.handle_, nullptr);
    }
    return *this;
  }

  void Apply(std::span<const std::uint8_t> frame) {
    CheckStatus(crdt_rga_apply(handle_, frame.data(), frame.size()));
  }

  [[nodiscard]] std::vector<std::uint8_t> Insert(std::size_t offset, std::string_view value) {
    crdt_buffer output{};
    CheckStatus(crdt_rga_insert(handle_, offset,
                                reinterpret_cast<const std::uint8_t*>(value.data()), value.size(),
                                &output));
    return TakeBuffer(output);
  }

  [[nodiscard]] std::vector<std::uint8_t> Delete(std::size_t offset, std::size_t count) {
    crdt_buffer output{};
    CheckStatus(crdt_rga_delete(handle_, offset, count, &output));
    return TakeBuffer(output);
  }

  [[nodiscard]] std::vector<std::uint8_t> State() const {
    crdt_buffer output{};
    CheckStatus(crdt_rga_state(handle_, &output));
    return TakeBuffer(output);
  }

  [[nodiscard]] ClockState Clock() const {
    crdt_clock_state state{};
    CheckStatus(crdt_rga_clock_state(handle_, &state));
    [[maybe_unused]] BufferOwner replica_id(state.replica_id);
    return ClockState{.replica_id = CopyBuffer(state.replica_id),
                      .wall_time = state.wall_time,
                      .logical = state.logical};
  }

  [[nodiscard]] std::string Text() const {
    const auto bytes = BufferFromText();
    return {bytes.begin(), bytes.end()};
  }

 private:
  class BufferOwner final {
   public:
    explicit BufferOwner(crdt_buffer value) noexcept : value_(value) {}
    ~BufferOwner() { crdt_buffer_free(value_); }

    BufferOwner(const BufferOwner&) = delete;
    BufferOwner& operator=(const BufferOwner&) = delete;

   private:
    crdt_buffer value_;
  };

  static crdt_rga* NewHandle(std::span<const std::uint8_t> replica_id,
                             const crdt_limits* limits = nullptr) {
    crdt_rga* handle = nullptr;
    const auto status = limits == nullptr
                            ? crdt_rga_new(replica_id.data(), replica_id.size(), &handle)
                            : crdt_rga_new_with_limits(replica_id.data(), replica_id.size(), limits,
                                                       &handle);
    CheckStatus(status);
    if (handle == nullptr) {
      throw Error(CRDT_INTERNAL);
    }
    return handle;
  }

  static crdt_rga* NewHandle(const ClockState& state) {
    crdt_rga* handle = nullptr;
    CheckStatus(crdt_rga_new_from_clock(state.replica_id.data(), state.replica_id.size(),
                                        state.wall_time, state.logical, &handle));
    if (handle == nullptr) {
      throw Error(CRDT_INTERNAL);
    }
    return handle;
  }

  static void CheckStatus(std::int32_t status) {
    if (status != CRDT_OK) {
      throw Error(status);
    }
  }

  static std::vector<std::uint8_t> CopyBuffer(const crdt_buffer& value) {
    if (value.len == 0) {
      return {};
    }
    if (value.data == nullptr) {
      throw Error(CRDT_INTERNAL);
    }
    return {value.data, value.data + value.len};
  }

  static std::vector<std::uint8_t> TakeBuffer(crdt_buffer value) {
    BufferOwner owner(value);
    return CopyBuffer(value);
  }

  [[nodiscard]] std::vector<std::uint8_t> BufferFromText() const {
    crdt_buffer output{};
    CheckStatus(crdt_rga_text(handle_, &output));
    return TakeBuffer(output);
  }

  crdt_rga* handle_{};
};

// LwwMap owns one opaque C ABI handle for the stable Go-compatible LWW-Map
// v1 TypeIDs 9/10. Values are opaque bytes; applications must authenticate a
// manifest that binds value schema, limits, document, and epoch before Apply.
class LwwMap final {
 public:
  explicit LwwMap(std::string_view replica_id)
      : LwwMap(std::span<const std::uint8_t>(
            reinterpret_cast<const std::uint8_t*>(replica_id.data()), replica_id.size())) {}

  LwwMap(std::string_view replica_id, const crdt_lww_map_limits& limits)
      : LwwMap(std::span<const std::uint8_t>(
                   reinterpret_cast<const std::uint8_t*>(replica_id.data()), replica_id.size()),
               limits) {}

  explicit LwwMap(std::span<const std::uint8_t> replica_id) : handle_(NewHandle(replica_id)) {}

  LwwMap(std::span<const std::uint8_t> replica_id, const crdt_lww_map_limits& limits)
      : handle_(NewHandle(replica_id, &limits)) {}

  explicit LwwMap(const ClockState& clock_state) : handle_(NewHandle(clock_state)) {}

  LwwMap(const ClockState& clock_state, const crdt_lww_map_limits& limits)
      : handle_(NewHandle(clock_state, &limits)) {}

  ~LwwMap() { crdt_lww_map_free(handle_); }

  LwwMap(const LwwMap&) = delete;
  LwwMap& operator=(const LwwMap&) = delete;

  LwwMap(LwwMap&& other) noexcept : handle_(std::exchange(other.handle_, nullptr)) {}

  LwwMap& operator=(LwwMap&& other) noexcept {
    if (this != &other) {
      crdt_lww_map_free(handle_);
      handle_ = std::exchange(other.handle_, nullptr);
    }
    return *this;
  }

  void Apply(std::span<const std::uint8_t> frame) {
    CheckStatus(crdt_lww_map_apply(handle_, frame.data(), frame.size()));
  }

  [[nodiscard]] std::vector<std::uint8_t> Set(std::string_view key,
                                               std::span<const std::uint8_t> value) {
    crdt_buffer output{};
    CheckStatus(crdt_lww_map_set(handle_, reinterpret_cast<const std::uint8_t*>(key.data()),
                                 key.size(), value.data(), value.size(), &output));
    return TakeBuffer(output);
  }

  [[nodiscard]] std::vector<std::uint8_t> Delete(std::string_view key) {
    crdt_buffer output{};
    CheckStatus(crdt_lww_map_delete(handle_, reinterpret_cast<const std::uint8_t*>(key.data()),
                                    key.size(), &output));
    return TakeBuffer(output);
  }

  [[nodiscard]] std::optional<std::vector<std::uint8_t>> Get(std::string_view key) const {
    crdt_buffer output{};
    std::uint8_t present = 0;
    CheckStatus(crdt_lww_map_get(handle_, reinterpret_cast<const std::uint8_t*>(key.data()),
                                 key.size(), &output, &present));
    BufferOwner owner(output);
    if (present == 0) {
      return std::nullopt;
    }
    if (present != 1) {
      throw Error(CRDT_INTERNAL);
    }
    return CopyBuffer(output);
  }

  [[nodiscard]] std::vector<std::string> Keys() const {
    const auto encoded = KeysBuffer();
    std::size_t cursor = 0;
    const auto count = ReadUvarint(encoded, &cursor);
    if (count > encoded.size() || count > static_cast<std::uint64_t>(SIZE_MAX)) {
      throw Error(CRDT_INTERNAL);
    }
    std::vector<std::string> keys;
    keys.reserve(static_cast<std::size_t>(count));
    for (std::uint64_t index = 0; index < count; ++index) {
      const auto length = ReadUvarint(encoded, &cursor);
      if (length > encoded.size() - cursor) {
        throw Error(CRDT_INTERNAL);
      }
      const auto length_size = static_cast<std::size_t>(length);
      keys.emplace_back(reinterpret_cast<const char*>(encoded.data() + cursor), length_size);
      cursor += length_size;
    }
    if (cursor != encoded.size()) {
      throw Error(CRDT_INTERNAL);
    }
    return keys;
  }

  [[nodiscard]] std::vector<std::uint8_t> State() const {
    crdt_buffer output{};
    CheckStatus(crdt_lww_map_state(handle_, &output));
    return TakeBuffer(output);
  }

  [[nodiscard]] ClockState Clock() const {
    crdt_clock_state state{};
    CheckStatus(crdt_lww_map_clock_state(handle_, &state));
    [[maybe_unused]] BufferOwner replica_id(state.replica_id);
    return ClockState{.replica_id = CopyBuffer(state.replica_id),
                      .wall_time = state.wall_time,
                      .logical = state.logical};
  }

 private:
  class BufferOwner final {
   public:
    explicit BufferOwner(crdt_buffer value) noexcept : value_(value) {}
    ~BufferOwner() { crdt_buffer_free(value_); }

    BufferOwner(const BufferOwner&) = delete;
    BufferOwner& operator=(const BufferOwner&) = delete;

   private:
    crdt_buffer value_;
  };

  static crdt_lww_map* NewHandle(std::span<const std::uint8_t> replica_id,
                                 const crdt_lww_map_limits* limits = nullptr) {
    crdt_lww_map* handle = nullptr;
    const auto status = limits == nullptr
                            ? crdt_lww_map_new(replica_id.data(), replica_id.size(), &handle)
                            : crdt_lww_map_new_with_limits(replica_id.data(), replica_id.size(),
                                                          limits, &handle);
    CheckStatus(status);
    if (handle == nullptr) {
      throw Error(CRDT_INTERNAL);
    }
    return handle;
  }

  static crdt_lww_map* NewHandle(const ClockState& state,
                                 const crdt_lww_map_limits* limits = nullptr) {
    crdt_lww_map* handle = nullptr;
    const auto status = limits == nullptr
                            ? crdt_lww_map_new_from_clock(state.replica_id.data(),
                                                          state.replica_id.size(), state.wall_time,
                                                          state.logical, &handle)
                            : crdt_lww_map_new_from_clock_with_limits(
                                  state.replica_id.data(), state.replica_id.size(), state.wall_time,
                                  state.logical, limits, &handle);
    CheckStatus(status);
    if (handle == nullptr) {
      throw Error(CRDT_INTERNAL);
    }
    return handle;
  }

  static void CheckStatus(std::int32_t status) {
    if (status != CRDT_OK) {
      throw Error(status);
    }
  }

  static std::vector<std::uint8_t> CopyBuffer(const crdt_buffer& value) {
    if (value.len == 0) {
      return {};
    }
    if (value.data == nullptr) {
      throw Error(CRDT_INTERNAL);
    }
    return {value.data, value.data + value.len};
  }

  static std::vector<std::uint8_t> TakeBuffer(crdt_buffer value) {
    BufferOwner owner(value);
    return CopyBuffer(value);
  }

  [[nodiscard]] std::vector<std::uint8_t> KeysBuffer() const {
    crdt_buffer output{};
    CheckStatus(crdt_lww_map_keys(handle_, &output));
    return TakeBuffer(output);
  }

  static std::uint64_t ReadUvarint(const std::vector<std::uint8_t>& value,
                                   std::size_t* cursor) {
    const auto start = *cursor;
    std::uint64_t result = 0;
    for (std::size_t shift = 0; shift < 10; ++shift) {
      if (*cursor >= value.size()) {
        throw Error(CRDT_INTERNAL);
      }
      const auto byte = value[(*cursor)++];
      if (shift == 9 && byte > 1) {
        throw Error(CRDT_INTERNAL);
      }
      result |= static_cast<std::uint64_t>(byte & 0x7FU) << (shift * 7U);
      if ((byte & 0x80U) == 0) {
        if (*cursor - start != UvarintSize(result)) {
          throw Error(CRDT_INTERNAL);
        }
        return result;
      }
    }
    throw Error(CRDT_INTERNAL);
  }

  static std::size_t UvarintSize(std::uint64_t value) noexcept {
    std::size_t size = 1;
    while (value >= 0x80U) {
      value >>= 7U;
      ++size;
    }
    return size;
  }

  crdt_lww_map* handle_{};
};

}  // namespace darkinno::crdt

#endif  // DARKINNO_CPP_CRDT_RGA_HPP
