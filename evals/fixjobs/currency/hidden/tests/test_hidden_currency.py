import unittest

from billing.customer import Customer
from billing.invoice import Invoice
from billing.money import format_money
from billing.receipt import receipt_line


class HiddenCurrencyTest(unittest.TestCase):
    def test_format(self):
        self.assertEqual(format_money(1234.5, "EUR"), "€1,234.50")
        self.assertEqual(format_money(2, "GBP"), "£2.00")
        self.assertEqual(format_money(1000, "JPY"), "JPY 1,000.00")
        self.assertEqual(format_money(5, "USD"), "$5.00")

    def test_invoice_and_receipt(self):
        c = Customer("Bo", "EUR")
        self.assertIn("Total: €10.00", Invoice(c, [("x", 10)]).render())
        self.assertEqual(receipt_line(c, 3), "Received €3.00 from Bo")
        self.assertIn("$1.00", Invoice(Customer("Cy"), [("y", 1)]).render())
