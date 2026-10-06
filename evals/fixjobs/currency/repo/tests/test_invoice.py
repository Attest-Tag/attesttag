import unittest

from billing.customer import Customer
from billing.invoice import Invoice


class InvoiceTest(unittest.TestCase):
    def test_usd(self):
        inv = Invoice(Customer("Ada"), [("Plan", 1200), ("Seats", 34.5)])
        self.assertIn("Total: $1,234.50", inv.render())
