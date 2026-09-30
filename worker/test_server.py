import unittest

from server import score

S = 1_000_000_000


def window(mags):
    n = len(mags)
    return {"t": [i * S // 100 for i in range(n)], "ax": [0.0] * n, "ay": mags, "az": [0.0] * n}


class ScoreTest(unittest.TestCase):
    def test_dip_then_impact_is_a_fall(self):
        self.assertEqual(score(window([1.0] * 50 + [0.2] * 30 + [4.0] * 5 + [1.0] * 115)), 1.0)

    def test_standing_still_is_not(self):
        self.assertEqual(score(window([1.0] * 200)), 0.0)

    def test_impact_without_dip_is_not(self):
        self.assertEqual(score(window([1.0] * 100 + [4.0] * 5 + [1.0] * 95)), 0.0)

    def test_impact_too_late_after_dip_is_not(self):
        self.assertEqual(score(window([0.2] * 10 + [1.0] * 150 + [4.0] * 5 + [1.0] * 35)), 0.0)


if __name__ == "__main__":
    unittest.main()
