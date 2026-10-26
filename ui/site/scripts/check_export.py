#!/usr/bin/env python3
"""Validate the generated public site, routes, fragments and bundled assets."""
from html.parser import HTMLParser
import json
from pathlib import Path
from urllib.parse import unquote, urljoin, urlsplit

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / 'out'
ORIGIN = 'https://tutor-mcp.dev'

class Page(HTMLParser):
    def __init__(self, path):
        super().__init__()
        self.path = path
        self.ids = set()
        self.duplicates = set()
        self.links = []
        self.assets = []
        self.headings = 0
        self.language = ''
        self.title = False
        self.description = False
    def handle_starttag(self, tag, attrs):
        attributes = dict(attrs)
        if tag == 'html': self.language = attributes.get('lang', '')
        if tag == 'title': self.title = True
        if tag == 'h1': self.headings += 1
        if tag == 'meta' and attributes.get('name') == 'description': self.description = bool(attributes.get('content'))
        if attributes.get('id'):
            identifier = attributes['id']
            if identifier in self.ids: self.duplicates.add(identifier)
            self.ids.add(identifier)
        if tag == 'a' and attributes.get('href'): self.links.append(attributes['href'])
        if tag in ('script', 'img', 'source') and attributes.get('src'): self.assets.append(attributes['src'])
        if tag == 'link' and attributes.get('rel') in ('stylesheet', 'icon', 'preload') and attributes.get('href'): self.assets.append(attributes['href'])

pages = {}
for path in OUT.rglob('*.html'):
    if any(part.startswith(('.env', '.ennv')) for part in path.relative_to(OUT).parts):
        raise AssertionError('A private configuration file is present in the export')
    parser = Page(path)
    parser.feed(path.read_text())
    pages[path] = parser
errors = []
for path, page in pages.items():
    relative = path.relative_to(OUT)
    route = '/' + str(relative.parent).replace('\\', '/') + '/' if path.name == 'index.html' and str(relative.parent) != '.' else '/' if path.name == 'index.html' else '/' + str(relative)
    if page.duplicates: errors.append(f'{relative}: duplicate ids {page.duplicates}')
    if page.headings != 1: errors.append(f'{relative}: expected one h1, got {page.headings}')
    if page.language != 'en' or not page.title or not page.description: errors.append(f'{relative}: incomplete English metadata')
    for href in page.links + page.assets:
        target = urlsplit(urljoin(ORIGIN + route, href))
        if target.netloc != 'tutor-mcp.dev' or target.scheme not in ('http', 'https'): continue
        decoded = unquote(target.path).lstrip('/')
        if any(part.startswith(('.env', '.ennv')) for part in Path(decoded).parts):
            errors.append(f'{relative}: private file linked')
            continue
        candidate = OUT / decoded
        if candidate.is_dir(): candidate /= 'index.html'
        if not candidate.is_file():
            errors.append(f'{relative}: missing local target {href}')
        elif target.fragment and candidate.suffix == '.html':
            other = pages.get(candidate)
            if other and unquote(target.fragment) not in other.ids: errors.append(f'{relative}: missing fragment {href}')

assert (OUT / '.htaccess').is_file(), 'Apache configuration missing'
assert (OUT / 'robots.txt').is_file(), 'robots.txt missing'
assert (OUT / 'sitemap.xml').is_file(), 'sitemap.xml missing'
for name in ('install.sh', 'install.ps1'):
    assert (OUT / name).read_bytes() == (ROOT.parent.parent / 'scripts' / name).read_bytes(), f'{name}: published installer differs from repository'
for path in OUT.rglob('*'):
    if path.name.startswith(('.env', '.ennv')): errors.append('Private configuration in generated site')
if errors:
    raise SystemExit('\n'.join(sorted(set(errors))))
print(f'Validated {len(pages)} HTML pages: English metadata, unique headings and IDs, local links, fragments, assets, Apache setup and installer copies.')
