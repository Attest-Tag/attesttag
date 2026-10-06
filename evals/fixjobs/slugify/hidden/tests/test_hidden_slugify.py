import unittest

from textutil import slugify


class HiddenSlugifyTest(unittest.TestCase):
    def test_basic(self):
        self.assertEqual(slugify("Hello, World!"), "hello-world")
        self.assertEqual(slugify("  --Already--slugged--  "), "already-slugged")

    def test_accents(self):
        self.assertEqual(slugify("Crème brûlée"), "creme-brulee")

    def test_length(self):
        s = slugify("word " * 40, max_length=20)
        self.assertLessEqual(len(s), 20)
        self.assertFalse(s.endswith("-"))
        self.assertTrue(s.startswith("word-word"))
