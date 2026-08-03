#include <cstdint>
#include <exception>
#include <iostream>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#include <darkinno/crdt_rga.hpp>

namespace {

using darkinno::crdt::Error;
using darkinno::crdt::LwwMap;
using darkinno::crdt::Rga;

void Require(bool condition, std::string_view message) {
  if (!condition) {
    throw std::runtime_error(std::string(message));
  }
}

std::vector<std::uint8_t> Hex(std::string_view value) {
  Require(value.size() % 2 == 0, "hex input must have complete bytes");
  std::vector<std::uint8_t> output;
  output.reserve(value.size() / 2);
  for (std::size_t index = 0; index < value.size(); index += 2) {
    const auto high = value[index];
    const auto low = value[index + 1];
    const auto nibble = [](char character) -> std::uint8_t {
      if (character >= '0' && character <= '9') {
        return static_cast<std::uint8_t>(character - '0');
      }
      if (character >= 'a' && character <= 'f') {
        return static_cast<std::uint8_t>(character - 'a' + 10);
      }
      throw std::runtime_error("invalid hexadecimal digit");
    };
    output.push_back(static_cast<std::uint8_t>((nibble(high) << 4U) | nibble(low)));
  }
  return output;
}

std::span<const std::uint8_t> Bytes(const std::vector<std::uint8_t>& value) {
  return std::span<const std::uint8_t>(value.data(), value.size());
}

}  // namespace

int main() {
  try {
    const auto vector = Hex("435244540114001201010205616c696365000100410101b20700c1d69811");
    Rga vector_reader("cpp-vector-reader");
    vector_reader.Apply(Bytes(vector));
    Require(vector_reader.Text() == "Aβ", "Go canonical vector projected the wrong text");

    const auto before = vector_reader.Text();
    auto corrupt = vector;
    corrupt.back() ^= 0x01U;
    try {
      vector_reader.Apply(Bytes(corrupt));
      throw std::runtime_error("corrupt frame was accepted");
    } catch (const Error& error) {
      Require(error.Code() == CRDT_INVALID_FRAME, "corrupt frame returned the wrong status");
    }
    Require(vector_reader.Text() == before, "corrupt frame changed visible text");

    Rga alice("cpp-alice");
    Rga bob("cpp-bob");
    Rga carol("cpp-carol");
    const auto initial = alice.Insert(0, "A");
    bob.Apply(Bytes(initial));
    carol.Apply(Bytes(initial));
    const auto bob_edit = bob.Insert(1, "B");
    const auto carol_edit = carol.Insert(1, "C");
    for (const auto* frame : {&carol_edit, &bob_edit, &bob_edit}) {
      alice.Apply(Bytes(*frame));
    }
    bob.Apply(Bytes(carol_edit));
    carol.Apply(Bytes(bob_edit));
    Require(alice.Text() == bob.Text() && alice.Text() == carol.Text(),
            "duplicate/reordered replicas did not converge");

    Rga recovered(alice.Clock());
    recovered.Apply(Bytes(alice.State()));
    Require(recovered.Text() == alice.Text(), "state recovery did not converge");
    const auto recovered_edit = recovered.Insert(recovered.Text().size(), "D");
    alice.Apply(Bytes(recovered_edit));
    Require(recovered.Text() == alice.Text(),
            "same-ID clock recovery produced a conflicting mutation");

    const crdt_limits rga_limits{
        .max_frame_bytes = 1U << 20U,
        .max_payload_bytes = 1U << 20U,
        .max_string_bytes = 64U << 10U,
        .max_nodes = 1,
        .max_tags = 1,
        .max_tombstones = 1,
        .max_pending_nodes = 1,
        .max_pending_bytes = 1,
    };
    Rga limited_rga("cpp-limited", rga_limits);
    [[maybe_unused]] const auto limited_initial = limited_rga.Insert(0, "A");
    const auto limited_state = limited_rga.State();
    Rga limited_recovered(limited_rga.Clock(), rga_limits);
    limited_recovered.Apply(Bytes(limited_state));
    Rga unbounded_rga("cpp-unbounded");
    const auto excess_delta = unbounded_rga.Insert(0, "B");
    const auto rga_limit_state = limited_recovered.State();
    const auto rga_limit_clock = limited_recovered.Clock();
    try {
      limited_recovered.Apply(Bytes(excess_delta));
      throw std::runtime_error("recovered RGA accepted a frame above its node limit");
    } catch (const Error& error) {
      Require(error.Code() == CRDT_RESOURCE_LIMIT, "RGA limit returned the wrong status");
    }
    Require(limited_recovered.State() == rga_limit_state,
            "RGA over-limit frame changed recovered state");
    const auto actual_rga_limit_clock = limited_recovered.Clock();
    Require(actual_rga_limit_clock.replica_id == rga_limit_clock.replica_id &&
                actual_rga_limit_clock.wall_time == rga_limit_clock.wall_time &&
                actual_rga_limit_clock.logical == rga_limit_clock.logical,
            "RGA over-limit frame changed recovered HLC");

    const auto map_vector = Hex("435244540109000e01016105616c69636501000101783c3edf37");
    LwwMap map_reader("cpp-map-vector-reader");
    map_reader.Apply(Bytes(map_vector));
    const auto map_value = map_reader.Get("a");
    Require(map_value.has_value() && *map_value == std::vector<std::uint8_t>{'x'},
            "Go LWW-Map vector projected the wrong value");
    Require(map_reader.State() == map_vector, "Go LWW-Map state did not re-encode canonically");

    LwwMap map_alice("cpp-map-alice");
    LwwMap map_bob("cpp-map-bob");
    LwwMap map_carol("cpp-map-carol");
    const auto map_initial = map_alice.Set("title", Bytes(std::vector<std::uint8_t>{'d', 'r', 'a', 'f', 't'}));
    map_bob.Apply(Bytes(map_initial));
    map_carol.Apply(Bytes(map_initial));
    const auto map_bob_edit = map_bob.Set("owner", Bytes(std::vector<std::uint8_t>{'b', 'o', 'b'}));
    const auto map_carol_edit = map_carol.Set("title", Bytes(std::vector<std::uint8_t>{'r', 'e', 'v', 'i', 'e', 'w', 'e', 'd'}));
    const auto map_delete = map_alice.Delete("obsolete");
    for (const auto* frame : {&map_carol_edit, &map_bob_edit, &map_delete, &map_bob_edit, &map_initial}) {
      map_alice.Apply(Bytes(*frame));
    }
    for (const auto* frame : {&map_delete, &map_carol_edit, &map_initial, &map_delete}) {
      map_bob.Apply(Bytes(*frame));
    }
    for (const auto* frame : {&map_bob_edit, &map_delete, &map_bob_edit, &map_initial}) {
      map_carol.Apply(Bytes(*frame));
    }
    Require(map_alice.Get("title") == std::optional<std::vector<std::uint8_t>>(
                                         std::vector<std::uint8_t>{'r', 'e', 'v', 'i', 'e', 'w', 'e', 'd'}),
            "LWW-Map did not retain the largest tag");
    Require(map_alice.Keys() == std::vector<std::string>{"owner", "title"},
            "LWW-Map keys were not canonical");
    Require(map_alice.State() == map_bob.State() && map_alice.State() == map_carol.State(),
            "LWW-Map duplicate/reordered replicas did not converge");
    LwwMap recovered_map(map_alice.Clock());
    recovered_map.Apply(Bytes(map_alice.State()));
    const auto recovered_map_edit = recovered_map.Set(
        "after-recovery", Bytes(std::vector<std::uint8_t>{'s', 'a', 'f', 'e'}));
    map_alice.Apply(Bytes(recovered_map_edit));
    Require(map_alice.State() == recovered_map.State(),
            "LWW-Map same-ID recovery produced a conflicting mutation");

    LwwMap concurrent_map("cpp-map-concurrent");
    std::vector<std::thread> writers;
    for (std::uint32_t index = 0; index < 4; ++index) {
      writers.emplace_back([&concurrent_map, index] {
        const auto key = "worker:" + std::to_string(index);
        const auto value = std::vector<std::uint8_t>{static_cast<std::uint8_t>('a' + index)};
        [[maybe_unused]] const auto delta = concurrent_map.Set(key, Bytes(value));
      });
    }
    for (auto& writer : writers) {
      writer.join();
    }
    Require(concurrent_map.Keys().size() == 4,
            "mutex-protected LWW-Map handle lost a concurrent local write");

    const crdt_lww_map_limits limited_map_limits{
        .max_frame_bytes = 1U << 20U,
        .max_payload_bytes = 1U << 20U,
        .max_string_bytes = 64U << 10U,
        .max_entries = 1,
        .max_tombstones = 1,
    };
    LwwMap limited_map("cpp-map-limited", limited_map_limits);
    [[maybe_unused]] const auto first_limited_delta =
        limited_map.Set("first", Bytes(std::vector<std::uint8_t>{'s', 'a', 'f', 'e'}));
    const auto limited_before = limited_map.State();
    try {
      [[maybe_unused]] const auto second_limited_delta =
          limited_map.Set("second", Bytes(std::vector<std::uint8_t>{'x'}));
      throw std::runtime_error("over-limit LWW-Map write was accepted");
    } catch (const Error& error) {
      Require(error.Code() == CRDT_RESOURCE_LIMIT, "LWW-Map limit returned the wrong status");
    }
    Require(limited_map.State() == limited_before, "over-limit LWW-Map write changed state");
    std::cout << "PASS: Go vectors, atomic rejection, three-replica convergence, and snapshot recovery for RGA and LWW-Map\n";
  } catch (const std::exception& error) {
    std::cerr << "FAIL: " << error.what() << '\n';
    return 1;
  }
}
