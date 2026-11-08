#!/usr/bin/env python3
"""Check the static handbook without network access or third-party packages.

This checks HTML links, asset references, fragment IDs and CSS url() references.
It does not validate external sites, Markdown heading slugs or backend routes.
Unreachable HTML pages are reported for review, not treated as broken links.
"""

import argparse
from collections import deque
from html.parser import HTMLParser
from pathlib import Path
import re
import sys
from urllib.parse import unquote, urlsplit

BACKEND_ROUTES = {'admin', 'admin-api', 'threads', 'runs', 'agents', 'assistants',
                  'store', 'vectors', 'internal', 'governance', 'mcp', 'healthz',
                  'readyz', 'metrics'}


class Page(HTMLParser):
    def __init__(self, text):
        super().__init__()
        self.ids, self.refs = set(), []
        self.feed(text)

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        for key in ('id', 'name') if tag == 'a' else ('id',):
            if attrs.get(key):
                self.ids.add(attrs[key])
        for key in ('href', 'src'):
            if attrs.get(key):
                self.refs.append((self.getpos()[0], attrs[key], tag == 'a'))


def check(site):
    site = site.resolve()
    pages = {p: Page(p.read_text(encoding='utf-8')) for p in sorted(site.rglob('*.html'))}
    errors, edges, checked = [], {p: set() for p in pages}, 0
    sources = [(p, page.refs) for p, page in pages.items()]
    for css in sorted(site.rglob('*.css')):
        text = re.sub(r'/\*.*?\*/', '', css.read_text(encoding='utf-8'), flags=re.S)
        refs = [(text.count('\n', 0, m.start()) + 1, m.group(2), False)
                for m in re.finditer(r'''url\(\s*(['"]?)(.*?)\1\s*\)''', text)]
        sources.append((css, refs))
    for source, refs in sources:
        for line, raw, navigation in refs:
            url = urlsplit(raw)
            if url.scheme and url.scheme not in ('http', 'https'):
                continue
            path = unquote(url.path)
            if url.netloc:
                if url.netloc in ('getrunkite.com', 'www.getrunkite.com'):
                    base = site
                    path = path.lstrip('/')
                elif url.netloc == 'getrunkite.github.io' and (path == '/runkite' or path.startswith('/runkite/')):
                    base, path = site, path[len('/runkite'):].lstrip('/')
                else:
                    continue
            else:
                base = source.parent
            first = path.lstrip('/').split('/', 1)[0]
            if (url.netloc or path.startswith('/')) and first in BACKEND_ROUTES:
                continue
            error = None
            if not url.netloc and path.startswith('/'):
                error = 'root-relative static URL breaks GitHub Pages; use a relative URL'
                base, path = site, path.lstrip('/')
            target = (base / path).resolve() if path or url.netloc else source
            if target.is_dir():
                target /= 'index.html'
            if not target.is_relative_to(site):
                error = 'target escapes the published site directory'
            elif not target.is_file():
                error = 'missing target: ' + str(target.relative_to(site))
            elif url.fragment and target in pages:
                fragment = unquote(url.fragment)
                if not fragment.startswith(':~:text=') and fragment not in pages[target].ids:
                    error = 'missing fragment: #' + fragment
            checked += 1
            if error:
                errors.append(f'{source.relative_to(site)}:{line}: {raw}: {error}')
            elif navigation and source in edges and target in pages:
                edges[source].add(target)
    seeds = [site / p for p in ('index.html', 'support/index.html', 'design/index.html')]
    pending, reachable = deque(p for p in seeds if p in pages), set()
    while pending:
        page = pending.popleft()
        if page not in reachable:
            reachable.add(page)
            pending.extend(edges[page] - reachable)
    return {'errors': errors, 'html_pages': len(pages), 'references': checked,
            'reachable_pages': len(reachable),
            'unreachable_pages': [str(p.relative_to(site)) for p in pages if p not in reachable]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--site-root', type=Path, default=Path(__file__).resolve().parents[1] / 'site')
    args = parser.parse_args()
    if not (args.site_root / 'index.html').is_file():
        parser.error('--site-root must contain index.html')
    result = check(args.site_root)
    for error in result['errors']:
        print(error, file=sys.stderr)
    print(f"Checked {result['html_pages']} HTML pages and {result['references']} local references; "
          f"{len(result['errors'])} broken links; {result['reachable_pages']} pages reachable from the catalogs.")
    if result['unreachable_pages']:
        print('Review unlinked pages (including intentional redirects): ' + ', '.join(result['unreachable_pages']))
    return bool(result['errors'])


if __name__ == '__main__':
    sys.exit(main())
