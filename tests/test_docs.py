import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
DOCS = [
    ROOT / "README.md",
    ROOT / "DEPLOY.md",
    ROOT / "docs" / "TECHNICAL.md",
    ROOT / "docs" / "capacity-planning.md",
]


class DocumentationTests(unittest.TestCase):
    def test_local_links_resolve(self):
        missing = []
        for path in DOCS:
            text = path.read_text(encoding="utf-8")
            for target in re.findall(r"\]\(([^)]+)\)", text):
                target = target.strip()
                if not target or target.startswith(
                    ("http://", "https://", "#", "mailto:")
                ):
                    continue
                local = target.split("#", 1)[0]
                if not (path.parent / local).resolve().exists():
                    missing.append((path.relative_to(ROOT), target))
        self.assertEqual(missing, [])

    def test_mermaid_fences_are_balanced(self):
        for path in DOCS:
            text = path.read_text(encoding="utf-8")
            opened = text.count("```mermaid")
            self.assertGreater(opened, 0, path)
            inside = False
            for line in text.splitlines():
                if line.strip() == "```mermaid":
                    self.assertFalse(inside, path)
                    inside = True
                elif line.strip() == "```" and inside:
                    inside = False
            self.assertFalse(inside, path)


if __name__ == "__main__":
    unittest.main()
