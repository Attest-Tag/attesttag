"""Small string helpers."""
import re


def collapse_spaces(text):
    """Every run of whitespace becomes one space; the ends are trimmed."""
    return re.sub(r"\s+", " ", text).strip()


def title_case(text):
    """Each word capitalised."""
    return " ".join(w[:1].upper() + w[1:].lower() for w in collapse_spaces(text).split(" "))
