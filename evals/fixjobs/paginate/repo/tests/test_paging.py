import unittest

from shop.paging import page_count


class PagingTest(unittest.TestCase):
    def test_exact_pages(self):
        self.assertEqual(page_count(20, 10), 2)
