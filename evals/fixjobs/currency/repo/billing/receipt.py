from billing.money import format_money


def receipt_line(customer, amount):
    return "Received {} from {}".format(format_money(amount), customer.name)
