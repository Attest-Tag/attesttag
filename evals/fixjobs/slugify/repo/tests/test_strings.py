import unittest

from textutil import collapse_spaces, title_case


class StringsTest(unittest.TestCase):
    def test_collapse(self):
        self.assertEqual(collapse_spaces("  a \n b  "), "a b")

    def test_title(self):
        self.assertEqual(title_case("hello WORLD"), "Hello World")
