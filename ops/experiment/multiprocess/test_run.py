import unittest
from run import collection_count


class DecodeTotals(unittest.TestCase):
    def test_referral_is_not_an_empty_total(self):
        self.assertIsNone(collection_count({'set': None}))
        self.assertEqual(collection_count({'set': {}}), 0)
        self.assertIsNone(collection_count({}))

    def test_sum_children(self):
        self.assertEqual(collection_count({'set': {'a': {'count': 7}, 'b': {'count': 3}}}), 10)


if __name__ == '__main__':
    unittest.main()
