#!/usr/bin/env python3
"""Institution journeys over the real HTTP boundary (driven by smoke-institution.sh).

1. The owner of an operator-provisioned institution accepts the owner
   invitation, enrolls TOTP and invites a pedagogy manager and a learner from
   the console. Both accept by email link; the manager enrolls TOTP.
2. Both sign in to an MCP client through /authorize (the manager with a TOTP
   code), exchange and refresh tenant-bound tokens. The manager publishes a
   formation and enrolls the learner through the admin API; the learner uses
   the MCP tutor.
3. A second institution is created through self-service signup.

Standard library only.
"""
import base64
import email
import glob
import hashlib
import hmac
import html
import http.client
import json
import os
import re
import secrets
import struct
import sys
import time
from urllib.parse import urlencode, urlsplit, parse_qs

BASE = os.environ['SMOKE_BASE_URL']
HOST, PORT = os.environ['SMOKE_CONNECT'].split(':')
TENANT = os.environ['SMOKE_TENANT']
OWNER_LINK = os.environ['SMOKE_OWNER_LINK']
MAIL_DIR = os.environ['SMOKE_MAIL_DIR']
IAT = os.environ['OAUTH_DCR_INITIAL_ACCESS_TOKEN']
REDIRECT = 'http://127.0.0.1:19090/callback'


def check(condition, message):
    if not condition:
        print('FAIL: ' + message, file=sys.stderr)
        sys.exit(1)


class Browser:
    """A minimal cookie-keeping client that follows same-site redirects."""

    def __init__(self):
        self.cookies = {}

    def request(self, method, path, body=None, content_type=None, bearer=None, headers=None, attempt=0):
        conn = http.client.HTTPConnection(HOST, int(PORT), timeout=60)
        h = {'Host': urlsplit(BASE).netloc, 'Accept': 'application/json, text/event-stream, text/html'}
        if self.cookies:
            h['Cookie'] = '; '.join(f'{k}={v}' for k, v in self.cookies.items())
        if content_type:
            h['Content-Type'] = content_type
        if bearer:
            h['Authorization'] = 'Bearer ' + bearer
        h.update(headers or {})
        conn.request(method, path, body, h)
        response = conn.getresponse()
        status, location, retry = response.status, response.getheader('Location'), response.getheader('Retry-After')
        for header, value in response.getheaders():
            if header.lower() != 'set-cookie':
                continue
            name, _, rest = value.partition('=')
            cookie_value = rest.split(';', 1)[0]
            if 'max-age=0' in value.lower() or cookie_value == '':
                self.cookies.pop(name, None)
            else:
                self.cookies[name] = cookie_value
        data = response.read().decode()
        conn.close()
        if status == 429 and attempt < 8:
            time.sleep(min(30, max(1, int(retry or '5'))))
            return self.request(method, path, body, content_type, bearer, headers, attempt + 1)
        return status, location, data

    def get(self, path):
        status, location, body = self.request('GET', path)
        while status in (302, 303) and location and location.startswith('/'):
            path = location
            status, location, body = self.request('GET', path)
        return status, path, body

    def post(self, path, values):
        status, location, body = self.request('POST', path, urlencode(values, doseq=True), 'application/x-www-form-urlencoded')
        if status in (302, 303) and location and location.startswith('/'):
            return self.get(location)
        return status, path, body


def local_path(link):
    parts = urlsplit(link)
    return parts.path + ('?' + parts.query if parts.query else '')


def field(pattern, page, what):
    match = re.search(pattern, page)
    check(match is not None, f'{what} not found in page')
    return html.unescape(match.group(1))


def csrf(page):
    return field(r'name="csrf_token"\s+value="([^"]+)"', page, 'CSRF token')


def totp(secret, at=None):
    key = base64.b32decode(secret + '=' * (-len(secret) % 8))
    counter = int((at or time.time()) // 30)
    digest = hmac.new(key, struct.pack('>Q', counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0f
    value = struct.unpack('>I', digest[offset:offset + 4])[0] & 0x7fffffff
    return f'{value % 1_000_000:06d}'


def fresh_totp(secret, used):
    """A code from a time step not used before (the server rejects replays)."""
    while True:
        step = int(time.time() // 30)
        if step not in used:
            used.add(step)
            return totp(secret)
        time.sleep(1)


def mail_link(path, seen):
    """The link of the oldest unread message pointing to path (sent in order)."""
    deadline = time.time() + 30
    while time.time() < deadline:
        for name in sorted(glob.glob(os.path.join(MAIL_DIR, '*.eml'))):
            if name in seen:
                continue
            with open(name) as handle:
                body = email.message_from_string(handle.read()).get_payload(decode=True).decode()
            match = re.search(re.escape(BASE + path) + r'\?token=[A-Za-z0-9_%-]+', body)
            if match:
                seen.add(name)
                return match.group(0)
        time.sleep(0.5)
    check(False, f'no email with a {path} link')


def enroll_totp(browser, page):
    secret = field(r'<span class="mono">([A-Z2-7]{32})</span>', page, 'TOTP secret')
    status, _, body = browser.post('/console/mfa/setup', {
        'csrf_token': csrf(page), 'credential_id': field(r'name="credential_id" value="([^"]+)"', page, 'credential'),
        'code': totp(secret)})
    check(status == 200 and 'Save your recovery codes' in body, f'TOTP enrollment {status}')
    return secret


def accept_invitation(link, password):
    browser = Browser()
    status, _, page = browser.get(local_path(link))
    check(status == 200, f'invitation page {status}')
    status, path, page = browser.post('/invite', {
        'csrf_token': csrf(page), 'token': field(r'name="token" value="([^"]+)"', page, 'invitation token'),
        'password': password, 'password_confirm': password})
    check(status == 200, f'invitation acceptance {status}')
    return browser, path, page


def claims(token):
    payload = token.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))


def sign_in(email_address, password, client_id, code=None):
    browser = Browser()
    verifier = secrets.token_urlsafe(48)
    params = {'response_type': 'code', 'client_id': client_id, 'redirect_uri': REDIRECT, 'scope': 'learner',
              'resource': BASE + '/mcp', 'state': 'smoke', 'code_challenge_method': 'S256',
              'code_challenge': base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')}
    status, _, page = browser.request('GET', '/authorize?' + urlencode(params))
    check(status == 200, f'{email_address}: authorize page {status}')
    form = dict(params, csrf_token=csrf(page), mode='login', email=email_address, password=password, approve_client='yes')
    if code:
        form['totp_code'] = code
    status, location, body = browser.request('POST', '/authorize', urlencode(form), 'application/x-www-form-urlencoded')
    check(status == 302, f'{email_address}: login {status} {re.sub(r"<[^>]+>", " ", body)[-300:]}')
    code_value = parse_qs(urlsplit(location).query)['code'][0]
    status, _, body = browser.request('POST', '/token', urlencode({
        'grant_type': 'authorization_code', 'code': code_value, 'client_id': client_id,
        'redirect_uri': REDIRECT, 'resource': BASE + '/mcp', 'code_verifier': verifier}),
        'application/x-www-form-urlencoded')
    check(status == 200, f'{email_address}: token exchange {status} {body[:120]}')
    tokens = json.loads(body)
    check(claims(tokens['access_token']).get('tid') == TENANT, f'{email_address}: token not bound to the institution')
    status, _, body = browser.request('POST', '/token', urlencode({
        'grant_type': 'refresh_token', 'refresh_token': tokens['refresh_token'],
        'client_id': client_id, 'resource': BASE + '/mcp'}), 'application/x-www-form-urlencoded')
    check(status == 200, f'{email_address}: refresh {status} {body[:120]}')
    return json.loads(body)['access_token']


def admin(token, method, path, payload=None, key=None):
    headers = {'Idempotency-Key': key} if key else {}
    status, _, body = Browser().request(method, path, json.dumps(payload) if payload is not None else None,
                                        'application/json' if payload is not None else None, token, headers)
    return status, (json.loads(body) if body.strip().startswith(('{', '[')) else body)


def parse(body):
    if body.startswith('event:') or body.startswith('data:'):
        body = next(line[5:].strip() for line in body.splitlines() if line.startswith('data:'))
    return json.loads(body)


def main():
    seen_mail = set()
    used_steps = {}

    # 1. Owner bootstrap from the operator's invitation link, then invitations.
    owner, path, page = accept_invitation(OWNER_LINK, 'owner-password-2026')
    check(path == '/console/mfa/setup', f'owner landed on {path}')
    owner_secret = enroll_totp(owner, page)
    used_steps[owner_secret] = {int(time.time() // 30)}
    status, _, page = owner.get('/console')
    check(status == 200 and 'owner@acme.test (you)' in page, f'owner console {status}')
    status, _, page = owner.post('/console/members/invite', {
        'csrf_token': csrf(page), 'emails': 'manager@acme.test', 'roles': ['pedagogy_manager', 'learner']})
    check(status == 200 and 'invitation(s) created' in page, f'invite manager {status}')
    status, _, page = owner.post('/console/members/invite', {
        'csrf_token': csrf(page), 'emails': 'alice@acme.test', 'roles': ['learner']})
    check(status == 200, f'invite learner {status}')
    manager_link = mail_link('/invite', seen_mail)
    alice_link = mail_link('/invite', seen_mail)

    manager, path, page = accept_invitation(manager_link, 'manager-password-2026')
    check(path == '/console/mfa/setup', f'manager landed on {path}')
    manager_secret = enroll_totp(manager, page)
    used_steps[manager_secret] = {int(time.time() // 30)}
    _, path, page = accept_invitation(alice_link, 'alice-password-2026')
    check('/mcp' in page, 'learner was not sent to her AI client')
    print('PASS: owner invitation, console TOTP enrollment and member invitations by email')

    # 2. AI-client sign-in with tenant-bound tokens.
    status, _, body = Browser().request('POST', '/register', json.dumps({
        'client_name': 'Institution smoke connector', 'redirect_uris': [REDIRECT],
        'token_endpoint_auth_method': 'none'}), 'application/json', headers={'Authorization': 'Bearer ' + IAT})
    check(status == 201, f'DCR registration {status}')
    client_id = json.loads(body)['client_id']
    manager_token = sign_in('manager@acme.test', 'manager-password-2026', client_id,
                            fresh_totp(manager_secret, used_steps[manager_secret]))
    learner_token = sign_in('alice@acme.test', 'alice-password-2026', client_id)
    print('PASS: members sign in (manager with TOTP), exchange and refresh tokens bound to their tenant')

    status, formation = admin(manager_token, 'POST', '/admin/catalog/formations', {'name': 'Go backend', 'description': 'Smoke formation'}, 'f1')
    check(status == 201, f'create formation {status}')
    version = formation['version']['ID']
    status, _ = admin(manager_token, 'POST', f'/admin/catalog/formation-versions/{version}/modules',
                      {'StableKey': 'm1', 'Title': 'Foundations', 'Position': 0}, 'm1')
    check(status == 201, f'create module {status}')
    for position, (key, label, prerequisites) in enumerate([('variables', 'Variables', []), ('functions', 'Functions', ['variables'])]):
        status, _ = admin(manager_token, 'POST', f'/admin/catalog/formation-versions/{version}/concepts',
                          {'ModuleStableKey': 'm1', 'StableKey': key, 'Label': label, 'Position': position,
                           'Prerequisites': prerequisites}, 'c-' + key)
        check(status == 201, f'create concept {key} {status}')
    status, _ = admin(manager_token, 'POST', f'/admin/catalog/formation-versions/{version}/publish', None, 'publish')
    check(status == 200, f'publish {status}')
    status, cohort = admin(manager_token, 'POST', '/admin/catalog/cohorts', {'formation_version_id': version, 'name': 'Smoke cohort', 'capacity': 10}, 'cohort')
    check(status == 201, f'create cohort {status}')
    alice_membership = claims(learner_token).get('membership_id')
    check(bool(alice_membership), 'learner token carries no membership id')
    status, _ = admin(manager_token, 'POST', f"/admin/catalog/cohorts/{cohort['cohort']['ID']}/enrollments",
                      {'membership_id': alice_membership, 'objectives': {}}, 'enroll')
    check(status == 201, f'enroll learner {status}')
    status, body = admin(learner_token, 'POST', '/admin/catalog/formations', {'name': 'x', 'description': 'x'}, 'forbidden')
    check(status == 403, f'learner reached the admin API: {status}')
    print('PASS: pedagogy manager publishes a formation and enrolls the learner')

    session, number = Browser(), 0

    def mcp(method, params):
        nonlocal number
        number += 1
        status, _, body = session.request('POST', '/mcp', json.dumps({'jsonrpc': '2.0', 'id': number, 'method': method, 'params': params}),
                                          'application/json', learner_token)
        check(status == 200, f'MCP {method} {status}')
        result = parse(body)
        check('error' not in result, f'MCP {method} error {result.get("error")}')
        return result['result']

    mcp('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {}, 'clientInfo': {'name': 'smoke', 'version': '1'}})
    check(len(mcp('tools/list', {})['tools']) > 0, 'no MCP tools')
    result = mcp('tools/call', {'name': 'init_domain', 'arguments': {'name': 'Smoke domain', 'concepts': ['fractions'],
                                                                     'prerequisites': {}, 'idempotency_key': 'smoke-domain'}})
    check(not result.get('isError'), 'init_domain failed')
    result = mcp('tools/call', {'name': 'get_next_activity', 'arguments': {}})
    check(not result.get('isError'), 'get_next_activity failed')
    print('PASS: institution learner uses the MCP tutor')

    # 3. Self-service signup of a second institution.
    founder = Browser()
    status, _, page = founder.get('/signup')
    check(status == 200, f'signup page {status}')
    status, _, page = founder.post('/signup', {
        'csrf_token': csrf(page), 'tenant_name': 'Beta School', 'tenant_slug': 'beta-school',
        'email': 'founder@beta.test', 'accept_terms': 'yes'})
    check(status == 202, f'signup request {status}')
    link = mail_link('/signup/complete', seen_mail)
    status, _, page = founder.get(local_path(link))
    check(status == 200, f'signup confirmation page {status}')
    status, path, page = founder.post('/signup/complete', {
        'csrf_token': csrf(page), 'token': field(r'name="token" value="([^"]+)"', page, 'signup token'),
        'password': 'founder-password-2026', 'password_confirm': 'founder-password-2026'})
    check(path == '/console/mfa/setup', f'founder landed on {path} ({status})')
    enroll_totp(founder, page)
    status, _, page = founder.get('/console')
    check(status == 200 and 'Beta School' in page, f'founder console {status}')
    print('PASS: self-service institution signup with email confirmation and TOTP')


if __name__ == '__main__':
    main()
