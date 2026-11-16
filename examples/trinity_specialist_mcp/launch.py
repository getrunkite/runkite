"""Launch the adjacent, explicitly built example binary. No shell or downloads."""

import os
import sys
from pathlib import Path

if __name__ == "__main__":
    binary = Path(__file__).resolve().with_name("runkite-specialist-mcp")
    if len(sys.argv) != 1 or not binary.is_file():
        sys.exit("Install the built runkite-specialist-mcp beside launch.py first.")
    os.execv(str(binary), [str(binary)])
