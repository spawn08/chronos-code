"""Module doc."""
import os, sys as system
from a.b import c as d, e
from . import rel
from x import *

__all__ = ["Foo", "helper"]

@dataclass
class Foo(Base, metaclass=Meta):
    """Foo doc."""
    x: int = 1

    def __init__(self, repo: Repo):
        self.repo = repo
        super().__init__()

    async def _run(self):
        os.path.join("a")
        helper(1)
        Bar()

def helper(n):
    # comment
    return n

MAX_SIZE = 10


class _Hidden:
    def __repr__(self):
        return "h"
