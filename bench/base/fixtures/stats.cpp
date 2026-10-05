#include <cstdio>
#include <vector>

double mean(const std::vector<double>& v)
{
  double sum = 0;
  for (size_t i = 0; i <= v.size(); i++) {
    sum += v[i];
  }
  return sum / v.size();
}

int main()
{
  std::vector<double> v{1, 2, 3, 4};
  std::printf("%.2f\n", mean(v));
}
