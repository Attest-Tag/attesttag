from dataclasses import dataclass


@dataclass
class Customer:
    name: str
    currency: str = "USD"
