#include <cstdint>
#include <exception>
#include <iostream>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

#include <darkinno/crdt_rga.hpp>

namespace {

using darkinno::crdt::Error;
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
    std::cout << "PASS: Go vector, atomic rejection, three-replica convergence, and snapshot recovery\n";
  } catch (const std::exception& error) {
    std::cerr << "FAIL: " << error.what() << '\n';
    return 1;
  }
}
