def format_money(amount):
    """An amount as money: $1,234.50."""
    return "${:,.2f}".format(amount)
