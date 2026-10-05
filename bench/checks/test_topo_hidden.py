"""Hidden check: the original topological_sort tests, which the hard task removes."""
import sys
import unittest

sys.path.insert(0, ".")
from alibuild_helpers.utilities import topological_sort  # noqa: E402


class HiddenTopologicalSortTest(unittest.TestCase):
    def test_resolve_dependency_chain(self):
        self.assertEqual(["c", "b", "a"], list(topological_sort({
            "a": {"package": "a", "requires": ["b"]},
            "b": {"package": "b", "requires": ["c"]},
            "c": {"package": "c", "requires": []},
        })))

    def test_diamond_dependency(self):
        self.assertEqual(["base", "mid2", "mid1", "top"], list(topological_sort({
            "top": {"package": "top", "requires": ["mid1", "mid2"]},
            "mid1": {"package": "mid1", "requires": ["base", "mid2"]},
            "mid2": {"package": "mid2", "requires": ["base"]},
            "base": {"package": "base", "requires": []},
        })))

    def test_every_dependency_first(self):
        specs = {
            "app": {"package": "app", "requires": ["lib1", "lib2", "lib3"]},
            "lib1": {"package": "lib1", "requires": ["core", "lib2"]},
            "lib2": {"package": "lib2", "requires": ["core", "lib3"]},
            "lib3": {"package": "lib3", "requires": ["core"]},
            "core": {"package": "core", "requires": []},
        }
        order = list(topological_sort(specs))
        self.assertEqual(sorted(order), sorted(specs))
        for pkg, spec in specs.items():
            for dep in spec["requires"]:
                self.assertLess(order.index(dep), order.index(pkg), f"{dep} after {pkg}")


if __name__ == "__main__":
    unittest.main()
