import unittest

from shop.paging import page_count, page_items


class HiddenPagingTest(unittest.TestCase):
    def test_rounds_up(self):
        self.assertEqual(page_count(25, 10), 3)
        self.assertEqual(page_count(0, 10), 0)

    def test_every_item_once(self):
        items = list(range(25))
        seen = []
        for p in range(1, page_count(len(items), 10) + 1):
            seen += page_items(items, p, 10)
        self.assertEqual(seen, items)
        self.assertEqual(page_items(items, 3, 10), [20, 21, 22, 23, 24])
        self.assertEqual(page_items(items, 4, 10), [])
