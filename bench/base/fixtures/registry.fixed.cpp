#include <deque>
#include <iostream>
#include <string>
#include <vector>

// Human readable name for a detector id.
std::string detectorName(int id)
{
  return "DET" + std::to_string(id);
}

// Keeps every registered id, plus a fast-access pointer to each of them.
struct Registry {
  std::deque<int> ids; // push_back never moves existing elements
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
