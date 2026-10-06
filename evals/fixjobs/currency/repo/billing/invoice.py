from billing.money import format_money


class Invoice:
    def __init__(self, customer, lines):
        self.customer = customer
        self.lines = lines  # [(description, amount)]

    def total(self):
        return sum(a for _, a in self.lines)

    def render(self):
        out = ["Invoice for " + self.customer.name]
        for desc, amount in self.lines:
            out.append("{}: {}".format(desc, format_money(amount)))
        out.append("Total: " + format_money(self.total()))
        return "\n".join(out)
