"""Paging over a list of products."""


def page_count(total, per_page):
    """How many pages total items fill at per_page items a page."""
    if per_page <= 0:
        raise ValueError("per_page must be positive")
    return total // per_page


def page_items(items, page, per_page):
    """The items on a 1-based page."""
    if page < 1 or page > page_count(len(items), per_page):
        return []
    start = (page - 1) * per_page
    return items[start:start + per_page - 1]
