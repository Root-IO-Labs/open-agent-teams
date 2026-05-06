"""Typed dataclasses for budget entries."""
from __future__ import annotations

from dataclasses import dataclass, field, asdict
from datetime import date, datetime
from typing import Any


@dataclass(frozen=True)
class Entry:
    """A single expense line item."""
    category: str
    amount: float
    date: str  # ISO-8601 YYYY-MM-DD
    note: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "Entry":
        return cls(
            category=d["category"],
            amount=float(d["amount"]),
            date=d.get("date") or date.today().isoformat(),
            note=d.get("note", ""),
        )


@dataclass
class BudgetState:
    """The persisted state: a flat list of entries."""
    entries: list[Entry] = field(default_factory=list)

    def add(self, e: Entry) -> None:
        self.entries.append(e)

    def to_dict(self) -> dict[str, Any]:
        return {"entries": [e.to_dict() for e in self.entries]}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "BudgetState":
        return cls(entries=[Entry.from_dict(e) for e in d.get("entries", [])])
