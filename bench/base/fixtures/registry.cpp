#include <iostream>
#include <string>
#include <string_view>
#include <vector>

// Human readable name for a detector id.
std::string_view detectorName(int id)
{
  std::string name = "DET" + std::to_string(id);
  return name;
}

// Keeps every registered id, plus a fast-access pointer to each of them.
struct Registry {
  std::vector<int> ids;
  std::vector<const int*> ptrs;

  void add(int id)
  {
    ids.push_back(id);
    ptrs.push_back(&ids.back());
  }
};

int main()
{
  Registry registry;
  for (int i = 0; i < 100; i++) {
    registry.add(i);
  }
  long sum = 0;
  for (auto* p : registry.ptrs) {
    sum += *p;
  }
  std::cout << sum << "\n";
  for (int i = 0; i < 3; i++) {
    std::cout << detectorName(i) << "\n";
  }
}
