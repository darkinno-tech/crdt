#include <chrono>
#include <cstdint>
#include <iostream>
#include <span>
#include <string>
#include <vector>

#include <darkinno/crdt_rga.hpp>

namespace {

std::span<const std::uint8_t> Bytes(const std::vector<std::uint8_t>& value) {
  return std::span<const std::uint8_t>(value.data(), value.size());
}

std::span<const std::uint8_t> Bytes(const std::string& value) {
  return std::span<const std::uint8_t>(reinterpret_cast<const std::uint8_t*>(value.data()),
                                       value.size());
}

}  // namespace

int main() {
  constexpr std::uint32_t kIterations = 8;
  auto total = std::chrono::nanoseconds::zero();
  for (std::uint32_t iteration = 0; iteration < kIterations; ++iteration) {
    darkinno::crdt::Rga writer("cpp-writer");
    darkinno::crdt::Rga reader("cpp-reader");
    std::string content;
    content.reserve(12 * 128);
    for (std::uint32_t index = 0; index < 128; ++index) {
      content += "rga-run-v2 ";
    }
    content.append(128, 'x');
    if (content.size() != 1536) {
      return 1;
    }
    const auto started = std::chrono::steady_clock::now();
    const auto delta = writer.Insert(0, content);
    reader.Apply(Bytes(delta));
    const auto state = reader.State();
    darkinno::crdt::Rga recovered("cpp-recovered");
    recovered.Apply(Bytes(state));
    const auto text = recovered.Text();
    if (text.empty()) {
      return 1;
    }
    total += std::chrono::duration_cast<std::chrono::nanoseconds>(
        std::chrono::steady_clock::now() - started);
  }
  std::cout << "cpp_rga_run_v2_insert_replicate_recover_ns_per_op="
            << total.count() / static_cast<std::int64_t>(kIterations) << '\n';

  total = std::chrono::nanoseconds::zero();
  for (std::uint32_t iteration = 0; iteration < kIterations; ++iteration) {
    darkinno::crdt::LwwMap writer("cpp-map-writer");
    darkinno::crdt::LwwMap reader("cpp-map-reader");
    const auto started = std::chrono::steady_clock::now();
    for (std::uint32_t index = 0; index < 128; ++index) {
      const auto key = "task:" + std::to_string(index);
      const auto value = "owner=reviewer;state=open;iteration=" + std::to_string(iteration) +
                         ";index=" + std::to_string(index);
      const auto delta = writer.Set(key, Bytes(value));
      reader.Apply(Bytes(delta));
    }
    const auto state = reader.State();
    darkinno::crdt::LwwMap recovered("cpp-map-recovered");
    recovered.Apply(Bytes(state));
    if (recovered.Keys().size() != 128) {
      return 1;
    }
    total += std::chrono::duration_cast<std::chrono::nanoseconds>(
        std::chrono::steady_clock::now() - started);
  }
  std::cout << "cpp_lww_map_v1_set_replicate_recover_128_ns_per_op="
            << total.count() / static_cast<std::int64_t>(kIterations) << '\n';
}
